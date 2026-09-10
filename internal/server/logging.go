// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64
var requestIDSeq atomic.Int64

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

// CreditPolicy converts token usage to an estimated credit cost when the
// upstream response does not expose an exact credit field.
type CreditPolicy struct {
	InputPer1K       float64
	OutputPer1K      float64
	CachedInputPer1K float64
}

// RequestLog is the persisted request metadata exposed by GET /requests.
// Prompts and response bodies are intentionally excluded.
type RequestLog struct {
	ID                    string  `json:"id"`
	CreatedAt             int64   `json:"created_at"`
	Route                 string  `json:"route"`
	Model                 string  `json:"model"`
	Mode                  string  `json:"mode"`
	Status                int     `json:"status"`
	AccountUID            string  `json:"account_uid,omitempty"`
	RequestedOutputTokens int64   `json:"requested_output_tokens"`
	InputTokens           int64   `json:"input_tokens"`
	OutputTokens          int64   `json:"output_tokens"`
	TotalTokens           int64   `json:"total_tokens"`
	CacheReadTokens       int64   `json:"cache_read_tokens"`
	CacheWriteTokens      int64   `json:"cache_write_tokens"`
	ToolCalls             int64   `json:"tool_calls"`
	TTFBMillis            int64   `json:"ttfb_millis"`
	LatencyMillis         int64   `json:"latency_millis"`
	CreditsConsumed       float64 `json:"credits_consumed"`
	CreditSource          string  `json:"credit_source"`
	Passthrough           bool    `json:"passthrough"`
	ErrorCode             string  `json:"error_code,omitempty"`
	ErrorMessage          string  `json:"error_message,omitempty"`
}

// RequestLogStore persists and reads request-level metadata.
type RequestLogStore interface {
	RecordRequest(RequestLog) error
	RecentRequests(limit int) ([]RequestLog, error)
}

// CreditMetricsStore persists credit totals alongside aggregate metrics.
type CreditMetricsStore interface {
	AddCredit(consumed float64, source string) error
}

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	id                    string
	start                 time.Time
	route                 string
	model                 string
	mode                  string // "stream" | "sync"
	uid                   string // 完整 uid，展示时只取前 8 位
	ttfb                  time.Duration
	toks                  int // <0 表示 usage 缺失 → 显示 "-"
	inputTokens           int
	totalTokens           int
	cacheRead             int
	cacheWrite            int
	toolCalls             int
	requestedOutputTokens int
	creditsConsumed       float64
	creditSource          string
	passthrough           bool
	errorCode             string
	errorMessage          string
	status                int
	creditPolicy          CreditPolicy
	metricsStore          MetricsStore
	requestLogStore       RequestLogStore

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
	return newChatStatWithOptions(now, body, stream, store, nil, CreditPolicy{}, false, "")
}

func newChatStatWithOptions(now time.Time, body []byte, stream bool, metricsStore MetricsStore, requestLogStore RequestLogStore, creditPolicy CreditPolicy, passthrough bool, route string) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{
		id:                    fmt.Sprintf("req_%d_%d", now.UnixNano(), requestIDSeq.Add(1)),
		start:                 now,
		route:                 route,
		model:                 parseModelFromBody(body),
		mode:                  mode,
		toks:                  -1,
		requestedOutputTokens: parseRequestedOutputTokens(body),
		passthrough:           passthrough,
		creditPolicy:          creditPolicy,
		metricsStore:          metricsStore,
		requestLogStore:       requestLogStore,
	}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	elapsed := time.Since(s.start)
	status := s.status
	if status == 0 {
		status = 500
	}
	success := status >= 200 && status < 300
	outputTokens := maxInt(s.toks, 0)
	if s.creditSource == "" {
		if estimated, ok := s.creditPolicy.Estimate(s.inputTokens, outputTokens, s.cacheRead); ok {
			s.creditsConsumed = estimated
			s.creditSource = "estimated"
		} else {
			s.creditSource = "unknown"
		}
	}
	requestMetrics.requests.Add(1)
	if success {
		requestMetrics.successes.Add(1)
	} else {
		requestMetrics.failures.Add(1)
	}
	requestMetrics.inputTokens.Add(int64(s.inputTokens))
	requestMetrics.outputTokens.Add(int64(outputTokens))
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
		_ = s.metricsStore.AddMetrics(1, boolInt(success), boolInt(!success), int64(s.inputTokens), int64(outputTokens), int64(s.totalTokens), int64(s.cacheRead), int64(s.cacheWrite), int64(s.toolCalls), s.ttfb.Milliseconds(), boolInt(s.ttfb > 0), elapsed.Milliseconds(), time.Now().Unix())
		if creditStore, ok := s.metricsStore.(CreditMetricsStore); ok && s.creditSource != "unknown" {
			_ = creditStore.AddCredit(s.creditsConsumed, s.creditSource)
		}
	}
	if s.requestLogStore != nil {
		_ = s.requestLogStore.RecordRequest(RequestLog{
			ID:                    s.id,
			CreatedAt:             s.start.Unix(),
			Route:                 defaultString(s.route, "/v1/chat/completions"),
			Model:                 s.model,
			Mode:                  s.mode,
			Status:                status,
			AccountUID:            s.uid,
			RequestedOutputTokens: int64(s.requestedOutputTokens),
			InputTokens:           int64(s.inputTokens),
			OutputTokens:          int64(outputTokens),
			TotalTokens:           int64(s.totalTokens),
			CacheReadTokens:       int64(s.cacheRead),
			CacheWriteTokens:      int64(s.cacheWrite),
			ToolCalls:             int64(s.toolCalls),
			TTFBMillis:            s.ttfb.Milliseconds(),
			LatencyMillis:         elapsed.Milliseconds(),
			CreditsConsumed:       s.creditsConsumed,
			CreditSource:          s.creditSource,
			Passthrough:           s.passthrough,
			ErrorCode:             s.errorCode,
			ErrorMessage:          s.errorMessage,
		})
	}
	logChatRow(s.ttfb, elapsed, s.model, s.mode, s.uid, status, s.toks, creditLog{value: s.creditsConsumed, source: s.creditSource, errorCode: s.errorCode})
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

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (p CreditPolicy) Estimate(inputTokens, outputTokens, cacheRead int) (float64, bool) {
	if p.InputPer1K <= 0 && p.OutputPer1K <= 0 && p.CachedInputPer1K <= 0 {
		return 0, false
	}
	if inputTokens <= 0 && outputTokens <= 0 {
		return 0, false
	}
	normalInput := maxInt(inputTokens-cacheRead, 0)
	credits := float64(normalInput)/1000*p.InputPer1K +
		float64(cacheRead)/1000*p.CachedInputPer1K +
		float64(outputTokens)/1000*p.OutputPer1K
	return credits, credits >= 0
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
	credits     float64
	hasCredits  bool
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
func (s *chatStatsReader) CreditUsage() (float64, bool) { return s.credits, s.hasCredits }

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
		if s.ttfb == 0 {
			// Preserve the fact that a data frame was observed even when the
			// reader and clock sample fall in the same tick.
			s.ttfb = time.Nanosecond
		}
	}
	var rawChunk map[string]any
	if json.Unmarshal([]byte(payload), &rawChunk) == nil {
		if credits, ok := extractCreditUsage(rawChunk); ok {
			s.credits = credits
			s.hasCredits = true
		}
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
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
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
	s.tokens = maxInts(chunk.Usage.CompletionTokens, chunk.Usage.OutputTokens)
	s.inputTokens = maxInts(chunk.Usage.PromptTokens, chunk.Usage.InputTokens)
	s.totalTokens = chunk.Usage.TotalTokens
	if s.totalTokens == 0 && (s.inputTokens > 0 || s.tokens > 0) {
		s.totalTokens = s.inputTokens + s.tokens
	}
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

func parseRequestedOutputTokens(body []byte) int {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return 0
	}
	for _, key := range []string{"max_completion_tokens", "max_output_tokens", "max_tokens"} {
		if value, ok := usageInt(obj[key]); ok && value > 0 {
			return value
		}
	}
	return 0
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	for _, key := range []string{"completion_tokens", "output_tokens"} {
		if value, ok := usageInt(u[key]); ok {
			return value
		}
	}
	return -1
}

func usageStats(resp map[string]any, s *chatStat) {
	if credits, ok := extractCreditUsage(resp); ok {
		s.creditsConsumed = credits
		s.creditSource = "upstream"
	}
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return
	}
	for _, key := range []string{"prompt_tokens", "input_tokens"} {
		if value, ok := usageInt(u[key]); ok {
			s.inputTokens = maxInts(s.inputTokens, value)
		}
	}
	for _, key := range []string{"completion_tokens", "output_tokens"} {
		if value, ok := usageInt(u[key]); ok {
			s.toks = maxInts(maxInt(s.toks, 0), value)
		}
	}
	if value, ok := usageInt(u["total_tokens"]); ok {
		s.totalTokens = value
	}
	if s.totalTokens == 0 && (s.inputTokens > 0 || s.toks > 0) {
		s.totalTokens = s.inputTokens + maxInt(s.toks, 0)
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

func extractCreditUsage(resp map[string]any) (float64, bool) {
	if resp == nil {
		return 0, false
	}
	creditKeys := []string{
		"credits_consumed", "credit_consumed", "credits_used", "credit_used", "used_credits", "consumed_credits", "cost_credits", "billable_credits", "credit_cost",
		"creditsConsumed", "creditConsumed", "creditsUsed", "creditUsed", "usedCredits", "consumedCredits", "costCredits", "billableCredits", "creditCost",
	}
	for _, key := range creditKeys {
		if value, ok := numberFloat(resp[key]); ok && validCreditValue(value) {
			return value, true
		}
	}
	for _, containerKey := range []string{"usage", "billing", "bill", "meter", "metrics", "meta", "metadata"} {
		container, ok := resp[containerKey].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range creditKeys {
			if value, ok := numberFloat(container[key]); ok && validCreditValue(value) {
				return value, true
			}
		}
	}
	return 0, false
}

func validCreditValue(value float64) bool {
	return value >= 0 && !math.IsInf(value, 0) && !math.IsNaN(value)
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

func numberFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		value, err := n.Float64()
		return value, err == nil
	case string:
		value, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return value, err == nil
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
type creditLog struct {
	value     float64
	source    string
	errorCode string
}

func logChatRow(ttfb, total time.Duration, model, mode, uid string, status int, toks int, credits ...creditLog) {
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
	creditField := ""
	if len(credits) > 0 && credits[0].source != "" && credits[0].source != "unknown" {
		creditField = fmt.Sprintf(" | credits=%.4g(%s)", credits[0].value, credits[0].source)
	}
	if len(credits) > 0 && credits[0].errorCode != "" {
		creditField += " | error=" + credits[0].errorCode
	}
	fmt.Fprintf(os.Stdout, "| #%03d | %s | %s | %s | %d | uid=%s | TTFB=%s | tok=%s | %stok/s | total=%.1fs%s |\n",
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
		creditField,
	)
}
