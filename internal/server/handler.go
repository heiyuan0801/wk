// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool             *pool.Pool
	Upstream         *upstream.Client
	APIKey           string // 空 = 不鉴权
	FrontendPassword string // 前端控制台密码；空 = 不启用前端密码
	ConfigPath       string // 配置文件路径，供控制台保存签到配置
	AuthDir          string
	Region           string
	LoginBin         string // OAuth 登录辅助程序路径
	CheckinNow       func()
	MaxRotate        int // 单请求最多换号次数，默认 3
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429 冷却，默认 60s
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m
}

// Handler 主路由。
type Handler struct {
	cfg        Config
	mux        *http.ServeMux
	sessionsMu sync.Mutex
	sessions   map[string]time.Time
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), sessions: make(map[string]time.Time)}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responses))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /stats", h.withAuth(h.stats))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.mux.HandleFunc("POST /admin/unlock", h.unlock)
	h.mux.HandleFunc("GET /admin/config", h.withFrontend(h.adminConfig))
	h.mux.HandleFunc("POST /admin/config", h.withFrontend(h.saveAdminConfig))
	h.mux.HandleFunc("POST /admin/checkin", h.withFrontend(h.runCheckin))
	h.mux.HandleFunc("POST /admin/account/url", h.withFrontend(h.accountURL))
	h.mux.HandleFunc("POST /admin/account/poll", h.withFrontend(h.accountPoll))
	// Static console assets are served from the image's frontend directory.
	h.mux.Handle("/", http.FileServer(http.Dir("frontend")))
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			validAPIKey := strings.HasPrefix(authz, "Bearer ") && strings.TrimPrefix(authz, "Bearer ") == h.cfg.APIKey
			if !validAPIKey && !h.frontendSession(r) {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) frontendSession(r *http.Request) bool {
	if h.cfg.FrontendPassword == "" {
		return false
	}
	c, err := r.Cookie("wb2api_frontend")
	if err != nil {
		return false
	}
	h.sessionsMu.Lock()
	defer h.sessionsMu.Unlock()
	expires, ok := h.sessions[c.Value]
	if !ok || time.Now().After(expires) {
		delete(h.sessions, c.Value)
		return false
	}
	return true
}

func (h *Handler) withFrontend(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.FrontendPassword != "" && !h.frontendSession(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "frontend_locked", "message": "frontend password required"}})
			return
		}
		next(w, r)
	}
}

func (h *Handler) unlock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req) != nil || h.cfg.FrontendPassword == "" || req.Password != h.cfg.FrontendPassword {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "密码错误"})
		return
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "session error", 500)
		return
	}
	token := hex.EncodeToString(b)
	h.sessionsMu.Lock()
	h.sessions[token] = time.Now().Add(24 * time.Hour)
	h.sessionsMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "wb2api_frontend", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) adminConfig(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ConfigPath == "" {
		writeJSON(w, 200, map[string]any{})
		return
	}
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	var c struct {
		Schedule struct {
			CheckinHours   []int `json:"checkin_hours"`
			KeepaliveHours []int `json:"keepalive_hours"`
		} `json:"schedule"`
		Region string `json:"region"`
	}
	if json.Unmarshal(raw, &c) != nil {
		writeJSON(w, 500, map[string]string{"error": "invalid config"})
		return
	}
	writeJSON(w, 200, c)
}

func (h *Handler) saveAdminConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CheckinHours   []int `json:"checkin_hours"`
		KeepaliveHours []int `json:"keepalive_hours"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	validHours := func(xs []int) bool {
		for _, x := range xs {
			if x < 0 || x > 23 {
				return false
			}
		}
		return true
	}
	if !validHours(req.CheckinHours) || !validHours(req.KeepaliveHours) {
		writeJSON(w, 400, map[string]string{"error": "小时必须在 0-23 之间"})
		return
	}
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		writeJSON(w, 500, map[string]string{"error": "invalid config"})
		return
	}
	doc["schedule"] = map[string]any{"checkin_hours": req.CheckinHours, "keepalive_hours": req.KeepaliveHours}
	out, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(h.cfg.ConfigPath, append(out, '\n'), 0644); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "restart_required": true})
}

func (h *Handler) runCheckin(w http.ResponseWriter, r *http.Request) {
	if h.cfg.CheckinNow == nil {
		writeJSON(w, 503, map[string]string{"error": "签到服务不可用"})
		return
	}
	go h.cfg.CheckinNow()
	writeJSON(w, 202, map[string]any{"ok": true, "message": "签到任务已启动"})
}

func (h *Handler) loginCommand(arg string) ([]byte, error) {
	bin := h.cfg.LoginBin
	if bin == "" {
		bin = "./login"
	}
	cmd := exec.Command(bin, arg)
	cmd.Dir = filepath.Dir(h.cfg.ConfigPath)
	return cmd.Output()
}

func (h *Handler) accountURL(w http.ResponseWriter, r *http.Request) {
	out, err := h.loginCommand("url")
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": fmt.Sprintf("登录初始化失败: %v", err)})
		return
	}
	writeJSON(w, 200, map[string]string{"url": strings.TrimSpace(string(out))})
}

func (h *Handler) accountPoll(w http.ResponseWriter, r *http.Request) {
	out, err := h.loginCommand("poll")
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": "登录尚未完成，请先在浏览器完成授权"})
		return
	}
	var result struct {
		AccessToken                         string `json:"access_token"`
		RefreshToken                        string `json:"refresh_token"`
		ExpiresIn                           int64  `json:"expires_in"`
		Domain, UID, EnterpriseID, Nickname string
	}
	if json.Unmarshal(out, &result) != nil || result.AccessToken == "" || result.UID == "" {
		writeJSON(w, 409, map[string]string{"error": "登录尚未完成，请完成授权后重试"})
		return
	}
	if err := os.MkdirAll(h.cfg.AuthDir, 0700); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	doc := map[string]any{"auth": map[string]any{"accessToken": result.AccessToken, "refreshToken": result.RefreshToken, "expiresAt": time.Now().Unix() + result.ExpiresIn, "domain": result.Domain}, "account": map[string]any{"uid": result.UID, "enterpriseId": result.EnterpriseID, "nickname": result.Nickname}}
	raw, _ := json.MarshalIndent(doc, "", "  ")
	path := filepath.Join(h.cfg.AuthDir, "workbuddy-"+result.UID+".json")
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if loaded, err := auth.LoadDir(h.cfg.AuthDir, h.cfg.Region); err == nil {
		h.cfg.Pool.SyncToDir(loaded)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "uid": result.UID, "nickname": result.Nickname})
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"healthy": healthy, "total": total})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":        h.cfg.Pool.List(),
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
		"metrics":         metricsSnapshot(),
	})
}

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, metricsSnapshot())
}

// 静态 CN 模型表（api-reference §5，动态接口失败时的回退）。
var staticModels = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview-agent", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：优先动态（缓存 1h），失败回退静态表。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelList 动态获取模型列表并包装成 OpenAI 格式（含 context_length）。
func (h *Handler) modelList() []map[string]any {
	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			entry := map[string]any{
				"id":                mi.ID,
				"object":            "model",
				"created":           1753600000,
				"owned_by":          "workbuddy",
				"context_length":    mi.ContextWindow,
				"max_output_tokens": mi.MaxTokens,
			}
			if mi.ContextWindow == 0 {
				entry["context_length"] = 131072 // 兜底
			}
			out = append(out, entry)
		}
		return out
	}
	return staticModels
}

// fetchDynamicModels 从池中任一健康账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接用静态表，避免反复打上游。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	acct := h.cfg.Pool.Pick()
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败惩罚该账号，避免下次 Pick 又选中同一个反复失败；lastFail 保持全局负缓存。
		h.cfg.Pool.NoteError(acct.UID)
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

// responses adapts the OpenAI Responses API to the existing Chat Completions
// execution path, preserving account rotation, retries, sticky sessions and
// upstream error handling in one place.
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	chatBody, stream, err := responsesToChat(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	chatReq := r.Clone(r.Context())
	chatReq.Body = io.NopCloser(bytes.NewReader(chatBody))
	if !stream {
		rec := httptest.NewRecorder()
		h.chatCompletions(rec, chatReq)
		if rec.Code < 200 || rec.Code >= 300 {
			copyResponse(w, rec)
			return
		}
		var chat map[string]any
		if json.Unmarshal(rec.Body.Bytes(), &chat) != nil {
			copyResponse(w, rec)
			return
		}
		writeJSON(w, http.StatusOK, chatToResponse(chat))
		return
	}
	sw := &responsesStreamWriter{header: make(http.Header), dst: w}
	h.chatCompletions(sw, chatReq)
}

func responsesToChat(raw []byte) ([]byte, bool, error) {
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, false, err
	}
	chat := make(map[string]any, len(in))
	for k, v := range in {
		switch k {
		case "input", "instructions", "stream", "max_output_tokens":
			continue
		}
		chat[k] = v
	}
	if model, ok := in["model"].(string); !ok || strings.TrimSpace(model) == "" {
		return nil, false, fmt.Errorf("model is required")
	}
	msgs := make([]map[string]any, 0, 4)
	if s, ok := in["instructions"].(string); ok && strings.TrimSpace(s) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": s})
	}
	if input, ok := in["input"]; ok {
		appendResponseInput(&msgs, input)
	}
	if len(msgs) == 0 {
		return nil, false, fmt.Errorf("input is required")
	}
	chat["messages"] = msgs
	stream, _ := in["stream"].(bool)
	chat["stream"] = stream
	if n, ok := in["max_output_tokens"]; ok {
		chat["max_tokens"] = n
	}
	out, err := json.Marshal(chat)
	return out, stream, err
}

func appendResponseInput(msgs *[]map[string]any, input any) {
	appendOne := func(role, text string) {
		if text != "" {
			*msgs = append(*msgs, map[string]any{"role": role, "content": text})
		}
	}
	switch v := input.(type) {
	case string:
		appendOne("user", v)
	case []any:
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				if s, ok := item.(string); ok {
					appendOne("user", s)
				}
				continue
			}
			role, _ := m["role"].(string)
			if role == "" {
				role = "user"
			}
			if content, ok := m["content"].(string); ok {
				appendOne(role, content)
				continue
			}
			if content, ok := m["content"].([]any); ok {
				var b strings.Builder
				for _, part := range content {
					if p, ok := part.(map[string]any); ok {
						if text, ok := p["text"].(string); ok {
							b.WriteString(text)
						}
					}
				}
				appendOne(role, b.String())
				continue
			}
			if text, ok := m["text"].(string); ok {
				appendOne(role, text)
			}
		}
	case map[string]any:
		role, _ := v["role"].(string)
		if role == "" {
			role = "user"
		}
		text, _ := v["content"].(string)
		appendOne(role, text)
	}
}

func chatToResponse(chat map[string]any) map[string]any {
	id := fmt.Sprintf("resp-%d", time.Now().UnixNano())
	if s, ok := chat["id"].(string); ok && s != "" {
		id = "resp-" + s
	}
	model, _ := chat["model"].(string)
	text := ""
	finish := "completed"
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			if m, ok := c["message"].(map[string]any); ok {
				text, _ = m["content"].(string)
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "stop" {
				finish = fr
			}
		}
	}
	item := map[string]any{"id": id + "-item", "type": "message", "status": finish, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
	output := []any{item}
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			if m, ok := c["message"].(map[string]any); ok {
				if calls, ok := m["tool_calls"].([]any); ok {
					for i, raw := range calls {
						if call, ok := raw.(map[string]any); ok {
							fn, _ := call["function"].(map[string]any)
							name, _ := fn["name"].(string)
							args, _ := fn["arguments"].(string)
							callID, _ := call["id"].(string)
							output = append(output, map[string]any{"id": callID, "type": "function_call", "status": "completed", "call_id": callID, "name": name, "arguments": args, "index": i})
						}
					}
				}
			}
		}
	}
	out := map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "status": finish, "model": model, "output": output, "output_text": text}
	if u, ok := chat["usage"]; ok {
		out["usage"] = responseUsage(u)
	}
	return out
}

// responseUsage normalizes the upstream Chat Completions usage shape into the
// field names used by the Responses API while retaining provider-specific data.
func responseUsage(raw any) any {
	u, ok := raw.(map[string]any)
	if !ok {
		return raw
	}
	out := make(map[string]any, len(u)+2)
	for k, v := range u {
		out[k] = v
	}
	copyNumber := func(dst string, keys ...string) {
		if _, exists := out[dst]; exists {
			return
		}
		for _, key := range keys {
			if v, exists := u[key]; exists {
				out[dst] = v
				return
			}
		}
	}
	copyNumber("input_tokens", "prompt_tokens")
	copyNumber("output_tokens", "completion_tokens")
	if _, exists := out["input_tokens_details"]; !exists {
		cached := 0
		for _, key := range []string{"prompt_cache_hit_tokens", "cache_read_input_tokens"} {
			if n, ok := numberInt(u[key]); ok {
				cached += n
			}
		}
		if cached > 0 {
			out["input_tokens_details"] = map[string]any{"cached_tokens": cached}
		}
	}
	return out
}

func numberInt(v any) (int, bool) {
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

type responseCapture interface {
	Header() http.Header
	WriteHeader(int)
	Write([]byte) (int, error)
}

func copyResponse(dst http.ResponseWriter, src *httptest.ResponseRecorder) {
	for k, vv := range src.Header() {
		for _, v := range vv {
			dst.Header().Add(k, v)
		}
	}
	dst.WriteHeader(src.Code)
	_, _ = dst.Write(src.Body.Bytes())
}

type responsesStreamWriter struct {
	header  http.Header
	dst     http.ResponseWriter
	buf     bytes.Buffer
	started bool
	id      string
	model   string
}

func (w *responsesStreamWriter) Header() http.Header { return w.header }
func (w *responsesStreamWriter) WriteHeader(code int) {
	if code >= 400 {
		w.dst.WriteHeader(code)
	}
}
func (w *responsesStreamWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		data := w.buf.Bytes()
		i := bytes.Index(data, []byte("\n\n"))
		if i < 0 {
			break
		}
		frame := string(data[:i])
		w.buf.Next(i + 2)
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			if payload == "[DONE]" {
				w.emit("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": w.id, "object": "response", "status": "completed", "output_text": ""}})
				continue
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(payload), &chunk) != nil {
				continue
			}
			if w.id == "" {
				w.id = "resp-" + fmt.Sprintf("%d", time.Now().UnixNano())
				w.model, _ = chunk["model"].(string)
				w.emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": w.id, "object": "response", "status": "in_progress", "model": w.model}})
			}
			if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
				if c, ok := choices[0].(map[string]any); ok {
					if d, ok := c["delta"].(map[string]any); ok {
						if text, ok := d["content"].(string); ok && text != "" {
							w.emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "response_id": w.id, "delta": text})
						}
						if calls, ok := d["tool_calls"].([]any); ok {
							for _, raw := range calls {
								if call, ok := raw.(map[string]any); ok {
									fn, _ := call["function"].(map[string]any)
									callID, _ := call["id"].(string)
									name, _ := fn["name"].(string)
									args, _ := fn["arguments"].(string)
									if name != "" {
										w.emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "response_id": w.id, "item": map[string]any{"type": "function_call", "call_id": callID, "name": name, "arguments": ""}})
									}
									if args != "" {
										w.emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "response_id": w.id, "call_id": callID, "delta": args})
									}
								}
							}
						}
					}
				}
			}
		}
	}
	return len(p), nil
}
func (w *responsesStreamWriter) emit(event string, v map[string]any) {
	raw, _ := json.Marshal(v)
	if !w.started {
		w.dst.Header().Set("Content-Type", "text/event-stream")
		w.dst.Header().Set("Cache-Control", "no-cache")
		w.dst.WriteHeader(http.StatusOK)
		w.started = true
	}
	_, _ = fmt.Fprintf(w.dst, "event: %s\ndata: %s\n\n", event, raw)
	if f, ok := w.dst.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	defer st.done()

	tried := map[string]bool{}
	var lastErr error

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	sessKey := ""
	stickyUID := ""
	if h.cfg.Session != nil {
		sessKey = session.ExtractKey(body)
		if sessKey != "" {
			if uid, ok := h.cfg.Session.Resolve(sessKey); ok {
				stickyUID = uid
			}
		}
	}

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先（PickByUID 已校验 health + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUID(stickyUID)
			if acct == nil {
				// 粘性号当前不可用（冷却/占满）→ 解绑，本次回落普通轮换。
				h.cfg.Session.Unbind(sessKey)
				stickyUID = ""
			}
		}
		if acct == nil {
			acct = h.cfg.Pool.PickExcluding(tried)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUID 往返（语义与 fail()/PickByUID-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				h.cfg.Session.Unbind(sessKey)
				stickyUID = ""
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("chat refresh uid=%s: save auth failed: %v", acct.UID, err)
			}
		}

		rc, status, respBody, terr := h.cfg.Upstream.ChatStream(acct, body)
		if terr != nil {
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 上游 client 已打 transport error 日志。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			st.status = status
			kind := upstream.Classify(status, string(respBody))
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			h.applyErrorPolicy(acct.UID, kind)
			fail(acct.UID)
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			_ = upstream.Stream(w, stats)
			st.ttfb = stats.TTFB()
			st.toks, _ = stats.Tokens()
			st.inputTokens, st.toks, st.totalTokens, st.cacheRead, st.cacheWrite, st.toolCalls = stats.UsageStats()
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		usageStats(resp, st)
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	st.status = http.StatusServiceUnavailable
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify），此处不再按原始 status 二次判断。
// 仅在 chatCompletions 轮转循环内调用：调用方已准备好 lastErr 并打算 continue 换号。
//
// 五条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate / ErrNotFound → Cooldown(CoolSoft)：即时软冷却（429/404）。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
