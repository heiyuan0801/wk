// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
package upstream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Aggregate 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI chat.completion 响应。
// 分片/半行由 bufio.Reader.ReadString 处理；遇到 "data: [DONE]" 结束。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
func Aggregate(r io.Reader) (map[string]any, error) {
	return AggregateWithID(r, "")
}

// AggregateWithID aggregates an upstream stream and uses fallbackID only when
// WorkBuddy did not include an identifier in any SSE frame.
func AggregateWithID(r io.Reader, fallbackID string) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model        string
		created          float64
		content          strings.Builder
		reasoning        strings.Builder
		refusal          strings.Builder
		role             = "assistant"
		finishReason     = "stop"
		usage            map[string]any
		gotAnyContent    bool
		validEvents      int
		toolCalls        = map[int]map[string]any{}
		toolOrder        []int
		identifierFields = map[string]any{}
	)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				// 上游显式结束：停止读取，DONE 之后的任何数据一律忽略。
				break
			} else {
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					if rawErr, ok := chunk["error"]; ok {
						return nil, fmt.Errorf("upstream returned an error: %v", rawErr)
					}
					// 有效事件计数：仅 JSON 解析成功的数据帧计入（解析失败沿用静默 continue）。
					validEvents++
					// Aggregate handles complete message snapshots itself so a
					// snapshot after deltas is not appended twice.
					copyResponseIDFields(identifierFields, chunk)
					if id == "" {
						id = ResponseID(chunk)
					}
					if v, ok := chunk["model"].(string); ok && model == "" {
						model = v
					}
					if v, ok := chunk["created"].(float64); ok && created == 0 {
						created = v
					}
					if u, ok := chunk["usage"].(map[string]any); ok {
						usage = u
					}
					if ch, ok := chunk["choices"].([]any); ok {
						for _, ci := range ch {
							c, _ := ci.(map[string]any)
							if c == nil {
								continue
							}
							if fr, ok := c["finish_reason"].(string); ok && fr != "" {
								finishReason = fr
							}
							if delta, ok := c["delta"].(map[string]any); ok {
								if r2, ok := delta["role"].(string); ok && r2 != "" {
									role = r2
								}
								if txt := contentText(delta["content"]); txt != "" {
									content.WriteString(txt)
									gotAnyContent = true
								}
								if rc := contentText(delta["reasoning_content"]); rc != "" {
									reasoning.WriteString(rc)
								}
								if text := contentText(delta["refusal"]); text != "" {
									refusal.WriteString(text)
								}
								if tcs, ok := delta["tool_calls"].([]any); ok {
									for _, tc := range tcs {
										call, ok := tc.(map[string]any)
										if !ok {
											continue
										}
										idx := 0
										if v, ok := call["index"].(float64); ok {
											idx = int(v)
										}
										merged, seen := toolCalls[idx]
										if !seen {
											merged = map[string]any{"index": idx}
											toolCalls[idx] = merged
											toolOrder = append(toolOrder, idx)
										}
										mergeToolCallDelta(merged, call)
									}
								}
								if _, modern := delta["tool_calls"]; !modern {
									for _, rawCall := range normalizedToolCalls(delta) {
										call, ok := rawCall.(map[string]any)
										if !ok {
											continue
										}
										idx := 0
										if n, ok := call["index"].(float64); ok {
											idx = int(n)
										}
										merged := toolCalls[idx]
										if merged == nil {
											merged = map[string]any{"index": idx}
											toolCalls[idx] = merged
											toolOrder = append(toolOrder, idx)
										}
										mergeToolCallDelta(merged, call)
									}
								}
							}
							// 有的上游把完整消息放在 message 里（非 delta）
							if msg, ok := c["message"].(map[string]any); ok {
								if txt := contentText(msg["content"]); txt != "" && !gotAnyContent {
									content.WriteString(txt)
									gotAnyContent = true
								}
								if rc := contentText(msg["reasoning_content"]); rc != "" {
									reasoning.WriteString(rc)
								}
								if text := contentText(msg["refusal"]); text != "" {
									refusal.WriteString(text)
								}
								if calls := normalizedToolCalls(msg); len(calls) > 0 {
									for _, rawCall := range calls {
										call, ok := rawCall.(map[string]any)
										if !ok {
											continue
										}
										idx := 0
										if n, ok := call["index"].(float64); ok {
											idx = int(n)
										}
										merged := toolCalls[idx]
										if merged == nil {
											merged = map[string]any{"index": idx}
											toolCalls[idx] = merged
											toolOrder = append(toolOrder, idx)
										}
										mergeToolCallDelta(merged, call)
									}
								}
							}
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if validEvents == 0 {
		// 上游返回 200 但没有任何有效数据事件（空流/只有 [DONE]/只有注释行）：
		// 不再合成空 content 的假成功响应，直接报错，由 handler 映射为 502 upstream_parse。
		return nil, fmt.Errorf("upstream stream contained no valid data events")
	}
	// Some reasoning models occasionally finish without a normal content
	// delta, while returning useful text only in refusal or reasoning_content.
	// Surface that text instead of producing a successful empty completion.
	if content.Len() == 0 && len(toolOrder) == 0 {
		fallback := strings.TrimSpace(refusal.String())
		if fallback == "" {
			fallback = strings.TrimSpace(reasoning.String())
		}
		if fallback == "" {
			return nil, fmt.Errorf("upstream completed response contained no content")
		}
		content.WriteString(fallback)
	}
	if id == "" {
		id = fallbackID
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	copyResponseIDFields(resp, identifierFields)
	// id is the canonical OpenAI field and always carries the exact upstream
	// request/record ID when WorkBuddy supplied one under an alternate name.
	resp["id"] = id
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// contentText accepts both the traditional string content and newer content
// part arrays used by some OpenAI-compatible upstreams.
func contentText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var out strings.Builder
		for _, part := range v {
			out.WriteString(contentText(part))
		}
		return out.String()
	case map[string]any:
		if text, _ := v["text"].(string); text != "" {
			return text
		}
		if text, _ := v["content"].(string); text != "" {
			return text
		}
	}
	return ""
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// sortInts 升序排序（避免引 sort 包只为三行）。
func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// normalizeFrame 以 OpenAI 流式规范白名单重建帧：仅保留标准字段，
// 剔除上游噪声（finish_reason:"" → null、空 content/refusal、空 tool_calls 列表、
// 空占位 function_call、顶层未知字段），空 delta 键一律省略，
// usage 缺失 → null，保证任意标准客户端按规范解析。
func normalizeFrame(obj map[string]any) map[string]any {
	return normalizeFrameWithID(obj, "")
}

func normalizeFrameWithID(obj map[string]any, fallbackID string) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	copyResponseIDFields(out, obj)
	if _, ok := out["object"]; !ok {
		out["object"] = "chat.completion.chunk"
	}
	if id := ResponseID(obj); id != "" {
		out["id"] = id
	} else if fallbackID != "" {
		out["id"] = fallbackID
	} else {
		out["id"] = "chatcmpl-wb2api"
	}
	if chs, ok := obj["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			nc := map[string]any{}
			if idx, ok := c["index"]; ok {
				nc["index"] = idx
			}
			delta := map[string]any{}
			if d, ok := c["delta"].(map[string]any); ok {
				if v, ok := d["role"].(string); ok && v != "" {
					delta["role"] = v
				}
				if v := contentText(d["content"]); v != "" {
					delta["content"] = v
				}
				if v := contentText(d["reasoning_content"]); v != "" {
					delta["reasoning_content"] = v
				}
				if v := contentText(d["refusal"]); v != "" {
					delta["refusal"] = v
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					delta["tool_calls"] = tcs
				}
				if fc, ok := d["function_call"]; ok && fc != nil {
					// 空占位 function_call（name/arguments 全空）视为噪声剔除
					keep := false
					if fcm, ok2 := fc.(map[string]any); ok2 {
						n, _ := fcm["name"].(string)
						a, _ := fcm["arguments"].(string)
						keep = n != "" || a != ""
					} else {
						keep = true
					}
					if keep {
						delta["function_call"] = fc
					}
				}
			}
			// A few compatible upstreams send the completed assistant message
			// in a stream frame instead of delta. Convert it to delta so the
			// downstream Responses adapter does not lose the only text.
			if msg, ok := c["message"].(map[string]any); ok {
				if text := contentText(msg["content"]); text != "" {
					delta["content"] = text
				}
				if text := contentText(msg["reasoning_content"]); text != "" {
					delta["reasoning_content"] = text
				}
				if text := contentText(msg["refusal"]); text != "" {
					delta["refusal"] = text
				}
			}
			if msg, ok := c["message"].(map[string]any); ok {
				if calls := normalizedToolCalls(msg); len(calls) > 0 {
					delta["tool_calls"] = calls
				}
			}
			if _, exists := delta["tool_calls"]; !exists {
				if calls := normalizedToolCalls(delta); len(calls) > 0 {
					delta["tool_calls"] = calls
				}
			}
			delete(delta, "function_call")
			nc["delta"] = delta
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				nc["finish_reason"] = fr
			} else {
				nc["finish_reason"] = nil
			}
			nchs = append(nchs, nc)
		}
		out["choices"] = nchs
	}
	if u, ok := obj["usage"]; ok {
		out["usage"] = u
	} else {
		out["usage"] = nil
	}
	return out
}

// Stream keeps the existing normalized SSE behavior.
func Stream(w http.ResponseWriter, r io.Reader) error {
	return StreamWithOptions(w, r, false)
}

// StreamWithOptions forwards upstream SSE with optional raw passthrough.
// Raw passthrough deliberately skips normalization and [DONE] repair so the
// client receives the upstream bytes as-is.
func StreamWithOptions(w http.ResponseWriter, r io.Reader, passthrough bool) error {
	return StreamWithOptionsAndID(w, r, passthrough, "")
}

// StreamWithOptionsAndID behaves like StreamWithOptions and uses fallbackID
// only for normalized frames that do not carry their own WorkBuddy ID.
func StreamWithOptionsAndID(w http.ResponseWriter, r io.Reader, passthrough bool, fallbackID string) error {
	if passthrough {
		return StreamRaw(w, r)
	}
	return streamNormalizedWithID(w, r, fallbackID)
}

// StreamRaw copies upstream SSE bytes without changing frames or sentinels.
func StreamRaw(w http.ResponseWriter, r io.Reader) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-WorkBuddy-Passthrough", "true")
	fl, _ := w.(http.Flusher)
	if n, err := io.Copy(w, r); err != nil {
		return err
	} else if n > 0 && fl != nil {
		fl.Flush()
	}
	return nil
}

// streamNormalized forwards upstream SSE frame-by-frame after normalizing it
// to the OpenAI-compatible shape.
func streamNormalized(w http.ResponseWriter, r io.Reader) error {
	return streamNormalizedWithID(w, r, "")
}

func streamNormalizedWithID(w http.ResponseWriter, r io.Reader, fallbackID string) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	var streamedContent, reasoning, refusal strings.Builder
	var streamID, streamModel string
	toolCallSeen := false
	fallbackEmitted := false
	emptyCompletion := false
	protocolError := ""
	var readErr error
	toolArgs := make(map[int]*strings.Builder)
	toolFinished := false

	writePayload := func(payload string) error {
		if _, err := io.WriteString(w, "data: "+payload+"\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	emitFallback := func() error {
		if fallbackEmitted || streamedContent.Len() > 0 || toolCallSeen {
			return nil
		}
		text := strings.TrimSpace(refusal.String())
		if text == "" {
			text = strings.TrimSpace(reasoning.String())
		}
		if text == "" {
			return nil
		}
		chunk := map[string]any{
			"id": streamID, "object": "chat.completion.chunk", "model": streamModel,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}},
			"usage":   nil,
		}
		raw, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		fallbackEmitted = true
		streamedContent.WriteString(text)
		return writePayload(string(raw))
	}

	// writeFrame 把 payload 按规范白名单重建后以 data: 帧写出并 flush。
	// 仅 JSON 解析成功时计数记为一次有效转发（JSON 解析失败照常降级原样写出，但不计数）。
	writeFrame := func(payload string) (int, error) {
		var obj map[string]any
		valid := 0
		if json.Unmarshal([]byte(payload), &obj) == nil {
			if rawErr, ok := obj["error"]; ok {
				protocolError = fmt.Sprint(rawErr)
				if err := writeRawPayload(w, payload, fl); err != nil {
					return 0, err
				}
				return 1, nil
			}
			frameFallbackID := streamID
			if frameFallbackID == "" {
				frameFallbackID = fallbackID
			}
			normalized := normalizeFrameWithID(obj, frameFallbackID)
			if id, _ := normalized["id"].(string); id != "" {
				streamID = id
				if h.Get("X-Request-Id") == "" {
					h.Set("X-Request-Id", id)
				}
			}
			if model, _ := normalized["model"].(string); model != "" {
				streamModel = model
			}
			hasFinish := false
			if choices, ok := normalized["choices"].([]any); ok {
				for _, rawChoice := range choices {
					choice, _ := rawChoice.(map[string]any)
					if choice == nil {
						continue
					}
					if finish, _ := choice["finish_reason"].(string); finish != "" {
						hasFinish = true
						// Some compatible providers use "stop" even when the
						// final chunk contains tool calls. Any non-empty finish
						// reason is therefore a terminal marker; the argument
						// JSON is validated below independently.
						toolFinished = true
					}
					delta, _ := choice["delta"].(map[string]any)
					if text := contentText(delta["content"]); text != "" {
						streamedContent.WriteString(text)
					}
					if text := contentText(delta["reasoning_content"]); text != "" {
						reasoning.WriteString(text)
					}
					if text := contentText(delta["refusal"]); text != "" {
						refusal.WriteString(text)
					}
					if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
						toolCallSeen = true
						for _, rawCall := range calls {
							call, ok := rawCall.(map[string]any)
							if !ok {
								continue
							}
							idx := 0
							if n, ok := call["index"].(float64); ok {
								idx = int(n)
							}
							fn, _ := call["function"].(map[string]any)
							arg, _ := fn["arguments"].(string)
							if toolArgs[idx] == nil {
								toolArgs[idx] = &strings.Builder{}
							}
							toolArgs[idx].WriteString(arg)
						}
					}
				}
			}
			if hasFinish {
				if err := emitFallback(); err != nil {
					return 0, err
				}
			}
			if raw, err := json.Marshal(normalized); err == nil {
				payload = string(raw)
			}
			valid = 1
		}
		if err := writePayload(payload); err != nil {
			return 0, err
		}
		return valid, nil
	}

	// writeRaw 原样写出一帧（绕过 normalizeFrame）并 flush。空流错误帧需保留 error 字段，
	// 不能被白名单剥掉，故不经 writeFrame 规范化。
	writeRaw := func(payload string) error {
		if _, werr := io.WriteString(w, "data: "+payload+"\n\n"); werr != nil {
			return werr
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	br := bufio.NewReaderSize(r, 64*1024)
	validFrames := 0
readLoop:
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "data:") && strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")) == "[DONE]":
			// 上游显式结束：停止读取，DONE 之后的任何数据（含垃圾帧）一律不再透传。
			// [DONE] 统一在循环结束后写出，保证恰好一个。
			break readLoop
		case strings.HasPrefix(trimmed, "data:"):
			n, werr := writeFrame(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
			validFrames += n
			if werr != nil {
				return werr
			}
			if protocolError != "" {
				break readLoop
			}
		case trimmed != "":
			// 注释/其他行：原样透传
			if _, werr := io.WriteString(w, line); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		// 空行（帧分隔）吞掉：本函数自产 "\n\n"
		if err != nil {
			if err == io.EOF {
				break
			}
			readErr = err
			break
		}
	}
	// 空流（0 有效帧）：先写一帧 error（绕过 normalizeFrame 原样保留 error 字段），
	// 再补 [DONE] 保证客户端能正常收尾，并返回非 nil error 供调用方记录。
	if readErr != nil {
		code, message := "upstream_stream_error", "upstream stream interrupted before completion"
		var timeout net.Error
		if errors.Is(readErr, context.DeadlineExceeded) || (errors.As(readErr, &timeout) && timeout.Timeout()) {
			code, message = "upstream_timeout", "upstream stream timed out before completion"
		}
		raw, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "upstream_error", "code": code, "message": message}})
		if err := writeRaw(string(raw)); err != nil {
			return err
		}
	} else if validFrames == 0 {
		_ = writeRaw(`{"error":{"message":"empty upstream stream","type":"upstream_error"}}`)
	} else if protocolError != "" {
		// The original error frame has already been forwarded. Only DONE remains.
	} else if err := emitFallback(); err != nil {
		return err
	} else if toolCallSeen && (!toolFinished || !toolArgumentsValid(toolArgs)) {
		_ = writeRaw(`{"error":{"message":"truncated tool call","type":"upstream_error"}}`)
		emptyCompletion = true
	} else if streamedContent.Len() == 0 && !toolCallSeen {
		_ = writeRaw(`{"error":{"message":"upstream completed response contained no content","type":"upstream_error"}}`)
		emptyCompletion = true
	}
	// 保证恰好写一个 [DONE]（上游漏发时兜底补上）。
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	if readErr != nil {
		return fmt.Errorf("upstream stream read failed: %w", readErr)
	}
	if validFrames == 0 {
		return fmt.Errorf("upstream stream contained no valid data events")
	}
	if emptyCompletion {
		return fmt.Errorf("upstream completed response contained no content")
	}
	if protocolError != "" {
		return fmt.Errorf("upstream returned an error: %s", protocolError)
	}
	return nil
}

func writeRawPayload(w io.Writer, payload string, fl http.Flusher) error {
	if _, err := io.WriteString(w, "data: "+payload+"\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	return nil
}

func toolArgumentsValid(args map[int]*strings.Builder) bool {
	if len(args) == 0 {
		return false
	}
	for _, value := range args {
		if !json.Valid([]byte(value.String())) {
			return false
		}
	}
	return true
}

// normalizedToolCalls accepts modern calls and the legacy single-function shape.
func normalizedToolCalls(message map[string]any) []any {
	if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
		out := make([]any, 0, len(calls))
		for i, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			copy := make(map[string]any, len(call)+1)
			for k, v := range call {
				copy[k] = v
			}
			if _, ok := copy["index"]; !ok {
				copy["index"] = float64(i)
			}
			out = append(out, copy)
		}
		return out
	}
	if fn, ok := message["function_call"].(map[string]any); ok {
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		if name != "" || args != "" {
			return []any{map[string]any{"index": float64(0), "type": "function", "function": fn}}
		}
	}
	return nil
}
