// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

var requestMetrics struct {
	requests, successes, failures          atomic.Int64
	inputTokens, outputTokens, totalTokens atomic.Int64
	cacheRead, cacheWrite, toolCalls       atomic.Int64
	ttfbMillis, ttfbSamples, latencyMillis atomic.Int64
	lastRequestUnix                        atomic.Int64
}

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start        time.Time
	model        string
	mode         string // "stream" | "sync"
	uid          string // 完整 uid，展示时只取前 8 位
	ttfb         time.Duration
	toks         int // <0 表示 usage 缺失 → 显示 "-"
	inputTokens  int
	totalTokens  int
	cacheRead    int
	cacheWrite   int
	toolCalls    int
	status       int
	metricsStore MetricsStore

	logged bool
}

// MetricsStore receives one aggregate delta when a request finishes.
type MetricsStore interface {
	AddMetrics(requests, successes, failures, inputTokens, outputTokens, totalTokens, cacheRead, cacheWrite, toolCalls, ttfbMillis, ttfbSamples, latencyMillis, lastRequestUnix int64) error
	SnapshotMetrics() map[string]any
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	return newChatStatWithStore(now, body, stream, nil)
}

func newChatStatWithStore(now time.Time, body []byte, stream bool, store MetricsStore) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1, metricsStore: store}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	elapsed := time.Since(s.start)
	requestMetrics.requests.Add(1)
	if s.status >= 200 && s.status < 300 {
		requestMetrics.successes.Add(1)
	} else {
		requestMetrics.failures.Add(1)
	}
	requestMetrics.inputTokens.Add(int64(s.inputTokens))
	requestMetrics.outputTokens.Add(int64(maxInt(s.toks, 0)))
	requestMetrics.totalTokens.Add(int64(s.totalTokens))
	requestMetrics.cacheRead.Add(int64(s.cacheRead))
	requestMetrics.cacheWrite.Add(int64(s.cacheWrite))
	requestMetrics.toolCalls.Add(int64(s.toolCalls))
	requestMetrics.latencyMillis.Add(elapsed.Milliseconds())
	if s.ttfb > 0 {
		requestMetrics.ttfbMillis.Add(s.ttfb.Milliseconds())
		requestMetrics.ttfbSamples.Add(1)
	}
	requestMetrics.lastRequestUnix.Store(time.Now().Unix())
	if s.metricsStore != nil {
		_ = s.metricsStore.AddMetrics(1, boolInt(s.status >= 200 && s.status < 300), boolInt(s.status < 200 || s.status >= 300), int64(s.inputTokens), int64(maxInt(s.toks, 0)), int64(s.totalTokens), int64(s.cacheRead), int64(s.cacheWrite), int64(s.toolCalls), s.ttfb.Milliseconds(), boolInt(s.ttfb > 0), elapsed.Milliseconds(), time.Now().Unix())
	}
	logChatRow(s.ttfb, elapsed, s.model, s.mode, s.uid, s.status, s.toks)
}

func boolInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func maxInt(v, floor int) int {
	if v < floor {
		return floor
	}
	return v
}

func maxInts(values ...int) int {
	max := 0
	for _, value := range values {
		if value > max {
			max = value
		}
	}
	return max
}
func metricsSnapshot() map[string]any {
	requests := requestMetrics.requests.Load()
	ttfbSamples := requestMetrics.ttfbSamples.Load()
	avgLatencyMS := int64(0)
	if requests > 0 {
		avgLatencyMS = requestMetrics.latencyMillis.Load() / requests
	}
	avgTTFBMS := int64(0)
	if ttfbSamples > 0 {
		avgTTFBMS = requestMetrics.ttfbMillis.Load() / ttfbSamples
	}
	return map[string]any{
		"requests": requests, "successes": requestMetrics.successes.Load(), "failures": requestMetrics.failures.Load(),
		"input_tokens": requestMetrics.inputTokens.Load(), "output_tokens": requestMetrics.outputTokens.Load(), "total_tokens": requestMetrics.totalTokens.Load(),
		"cache_read_tokens": requestMetrics.cacheRead.Load(), "cache_write_tokens": requestMetrics.cacheWrite.Load(), "tool_calls": requestMetrics.toolCalls.Load(),
		"avg_ttfb_ms": avgTTFBMS, "avg_latency_ms": avgLatencyMS, "last_request_at": requestMetrics.lastRequestUnix.Load(),
	}
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br          *bufio.Reader
	start       time.Time
	ttfb        time.Duration
	seen        bool // 已见过首个 data 帧（TTFB 只记一次）
	hasUsage    bool // 末帧是否带 usage
	tokens      int
	inputTokens int
	totalTokens int
	cacheRead   int
	cacheWrite  int
	toolCalls   int
	toolCallIDs map[string]struct{}
	pend        []byte // 已读未返回的行缓存
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since, toolCallIDs: make(map[string]struct{})}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.tokens, s.hasUsage }
func (s *chatStatsReader) UsageStats() (input, output, total, cacheRead, cacheWrite, toolCalls int) {
	return s.inputTokens, s.tokens, s.totalTokens, s.cacheRead, s.cacheWrite, s.toolCalls
}

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	var toolChunk struct {
		Choices []struct {
			Delta struct {
				ToolCalls []struct {
					ID    string `json:"id"`
					Index int    `json:"index"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(payload), &toolChunk) == nil {
		if s.toolCallIDs == nil {
			s.toolCallIDs = make(map[string]struct{})
		}
		for _, c := range toolChunk.Choices {
			for _, call := range c.Delta.ToolCalls {
				// IDs are commonly present only in the first delta; index remains
				// stable across every fragment of the same tool call.
				key := fmt.Sprintf("index:%d", call.Index)
				if _, seen := s.toolCallIDs[key]; !seen {
					s.toolCallIDs[key] = struct{}{}
					s.toolCalls++
				}
			}
		}
	}
	var chunk struct {
		Usage *struct {
			PromptTokens             int `json:"prompt_tokens"`
			CompletionTokens         int `json:"completion_tokens"`
			TotalTokens              int `json:"total_tokens"`
			PromptCacheHitTokens     int `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens    int `json:"prompt_cache_miss_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			PromptTokensDetails      struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			InputTokensDetails struct {
				CachedTokens             int `json:"cached_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheWriteTokens         int `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		return
	}
	s.hasUsage = true
	s.tokens = chunk.Usage.CompletionTokens
	s.inputTokens = chunk.Usage.PromptTokens
	s.totalTokens = chunk.Usage.TotalTokens
	s.cacheRead = maxInts(chunk.Usage.PromptCacheHitTokens, chunk.Usage.CacheReadInputTokens, chunk.Usage.PromptTokensDetails.CachedTokens, chunk.Usage.InputTokensDetails.CachedTokens)
	s.cacheWrite = maxInts(chunk.Usage.PromptCacheMissTokens, chunk.Usage.CacheCreationInputTokens, chunk.Usage.InputTokensDetails.CacheCreationInputTokens, chunk.Usage.InputTokensDetails.CacheWriteTokens)
	s.totalTokens = chunk.Usage.TotalTokens
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

func usageStats(resp map[string]any, s *chatStat) {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return
	}
	for key, dst := range map[string]*int{"prompt_tokens": &s.inputTokens, "input_tokens": &s.inputTokens, "completion_tokens": &s.toks, "output_tokens": &s.toks, "total_tokens": &s.totalTokens} {
		if v, ok := usageInt(u[key]); ok {
			*dst = v
		}
	}
	cacheRead := 0
	for _, key := range []string{"prompt_cache_hit_tokens", "cache_read_input_tokens"} {
		if v, ok := usageInt(u[key]); ok {
			cacheRead = maxInts(cacheRead, v)
		}
	}
	cacheWrite := 0
	for _, key := range []string{"prompt_cache_miss_tokens", "cache_creation_input_tokens"} {
		if v, ok := usageInt(u[key]); ok {
			cacheWrite = maxInts(cacheWrite, v)
		}
	}
	// Responses API and several OpenAI-compatible providers expose cache usage
	// in nested *_tokens_details objects rather than flat fields.
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if details, ok := u[key].(map[string]any); ok {
			if v, ok := usageInt(details["cached_tokens"]); ok {
				cacheRead = maxInts(cacheRead, v)
			}
		}
	}
	if details, ok := u["input_tokens_details"].(map[string]any); ok {
		for _, key := range []string{"cache_creation_input_tokens", "cache_write_tokens"} {
			if v, ok := usageInt(details[key]); ok {
				cacheWrite = maxInts(cacheWrite, v)
			}
		}
	}
	s.cacheRead = cacheRead
	s.cacheWrite = cacheWrite
	if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			if m, ok := c["message"].(map[string]any); ok {
				if calls, ok := m["tool_calls"].([]any); ok {
					s.toolCalls = len(calls)
				}
			}
		}
	}
}

func usageInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
func uidPrefix(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
// toks<0 表示 usage 缺失，显示 "-"。
func logChatRow(ttfb, total time.Duration, model, mode, uid string, status int, toks int) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	if len(model) > 11 {
		model = model[:11]
	}
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1f", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	fmt.Fprintf(os.Stdout, "| #%03d | %s | %s | %s | %d | uid=%s | TTFB=%s | tok=%s | %stok/s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		uidPrefix(uid),
		ttfbMS,
		tokField,
		tokpsField,
		total.Seconds(),
	)
}
