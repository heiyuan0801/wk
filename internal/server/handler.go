// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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
	Pool               *pool.Pool
	Upstream           *upstream.Client
	APIKey             string // 空 = 不鉴权
	FrontendPassword   string // 前端控制台密码；空 = 不启用前端密码
	ConfigPath         string // 配置文件路径，供控制台保存签到配置
	AuthDir            string
	Region             string
	LoginBin           string // OAuth 登录辅助程序路径
	CheckinNow         func()
	CreditRefreshNow   func()
	UpdateSchedule     func(checkinHours, keepaliveHours []int)
	UpdateLogRetention func(days int)
	MaxRotate          int // 单请求最多换号次数，默认 3
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode       string
	SoftCooldown    time.Duration // 429 冷却，默认 60s
	RefreshSkew     time.Duration // token 提前刷新窗口，默认 10m
	ResponseStore   ResponseStore
	MetricsStore    MetricsStore
	RequestLogStore RequestLogStore
	CompletionStore CompletionStore // optional atomic writer replacing separate metric/log writes
	CreditPolicy    CreditPolicy
	Passthrough     bool
}

// ResponseStore is the optional Redis-backed persistence used by
// previous_response_id across restarts and replicas.
type ResponseStore interface {
	SaveResponse(id string, data []byte, ttl time.Duration)
	LoadResponse(id string) ([]byte, bool)
}

// Handler 主路由。
type Handler struct {
	cfg             Config
	mux             *http.ServeMux
	sessionsMu      sync.Mutex
	sessions        map[string]time.Time
	configMu        sync.Mutex
	responsesMu     sync.Mutex
	responseHistory map[string]storedResponse
	responseBytes   int
	unlockRateMu    sync.Mutex
	unlockAttempts  map[string]unlockAttempt
}

type unlockAttempt struct {
	windowStart  time.Time
	failures     int
	blockedUntil time.Time
}

type storedResponse struct {
	messages  []map[string]any
	parentID  string
	routeKey  string
	expiresAt time.Time
	size      int
}

type storedResponseWire struct {
	Messages []map[string]any `json:"messages"`
	ParentID string           `json:"parent_id,omitempty"`
	RouteKey string           `json:"route_key"`
}

const (
	responseHistoryTTL      = time.Hour
	maxResponseHistory      = 1024
	maxResponseHistoryBytes = 64 << 20
	maxRequestBodyBytes     = 8 << 20
	unlockFailureLimit      = 5
	unlockFailureWindow     = time.Minute
	unlockBlockDuration     = 5 * time.Minute
)

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
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), sessions: make(map[string]time.Time), responseHistory: make(map[string]storedResponse), unlockAttempts: make(map[string]unlockAttempt)}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responses))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	// Some OpenAI-compatible clients append endpoint paths to a bare base URL.
	// Route these aliases through exactly the same authentication and handlers.
	h.mux.HandleFunc("POST /chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /responses", h.withAuth(h.responses))
	h.mux.HandleFunc("GET /models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /stats", h.withAuth(h.stats))
	h.mux.HandleFunc("GET /requests", h.withAuth(h.requests))
	h.mux.HandleFunc("GET /v1/requests", h.withAuth(h.requests))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.mux.HandleFunc("POST /admin/unlock", h.unlock)
	h.mux.HandleFunc("GET /admin/config", h.withFrontend(h.adminConfig))
	h.mux.HandleFunc("POST /admin/config", h.withFrontend(h.saveAdminConfig))
	h.mux.HandleFunc("POST /admin/checkin", h.withFrontend(h.runCheckin))
	h.mux.HandleFunc("POST /admin/credits/refresh", h.withFrontend(h.refreshCredits))
	h.mux.HandleFunc("POST /admin/account/url", h.withFrontend(h.accountURL))
	h.mux.HandleFunc("POST /admin/account/poll", h.withFrontend(h.accountPoll))
	h.mux.HandleFunc("POST /admin/account/{uid}/enable", h.withFrontend(h.enableAccount))
	h.mux.HandleFunc("POST /admin/account/{uid}/disable", h.withFrontend(h.disableAccount))
	h.mux.HandleFunc("DELETE /admin/account/{uid}", h.withFrontend(h.deleteAccount))
	// Static console assets are served from the image's frontend directory.
	h.mux.Handle("/", http.FileServer(http.Dir("frontend")))
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" && !h.validAPIKey(r) && !h.frontendSession(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func (h *Handler) validAPIKey(r *http.Request) bool {
	if h.cfg.APIKey == "" {
		return false
	}
	authz := r.Header.Get("Authorization")
	return strings.HasPrefix(authz, "Bearer ") && strings.TrimPrefix(authz, "Bearer ") == h.cfg.APIKey
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
		// Admin endpoints accept either the configured API key or the frontend
		// unlock cookie. If either credential is configured, require one of them.
		if (h.cfg.APIKey != "" || h.cfg.FrontendPassword != "") &&
			!h.validAPIKey(r) && !h.frontendSession(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "frontend_locked", "message": "frontend password or API key required"}})
			return
		}
		next(w, r)
	}
}

func (h *Handler) unlock(w http.ResponseWriter, r *http.Request) {
	if retryAfter := h.unlockRetryAfter(unlockClientKey(r)); retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "尝试次数过多，请稍后重试"})
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	clientKey := unlockClientKey(r)
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req) != nil || h.cfg.FrontendPassword == "" || req.Password != h.cfg.FrontendPassword {
		h.recordUnlockFailure(clientKey)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "密码错误"})
		return
	}
	h.clearUnlockFailures(clientKey)
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

func unlockClientKey(r *http.Request) string {
	if host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr)); err == nil && host != "" {
		return host
	}
	if raw := strings.TrimSpace(r.RemoteAddr); raw != "" {
		return raw
	}
	return "unknown"
}

func (h *Handler) unlockRetryAfter(key string) int {
	if h.cfg.FrontendPassword == "" {
		return 0
	}
	now := time.Now()
	h.unlockRateMu.Lock()
	defer h.unlockRateMu.Unlock()
	h.cleanupUnlockAttemptsLocked(now)
	attempt := h.unlockAttempts[key]
	if now.Before(attempt.blockedUntil) {
		seconds := int(time.Until(attempt.blockedUntil).Seconds())
		if seconds < 1 {
			seconds = 1
		}
		return seconds
	}
	return 0
}

func (h *Handler) recordUnlockFailure(key string) {
	if h.cfg.FrontendPassword == "" {
		return
	}
	now := time.Now()
	h.unlockRateMu.Lock()
	defer h.unlockRateMu.Unlock()
	h.cleanupUnlockAttemptsLocked(now)
	attempt := h.unlockAttempts[key]
	if attempt.windowStart.IsZero() || now.Sub(attempt.windowStart) >= unlockFailureWindow {
		attempt = unlockAttempt{windowStart: now}
	}
	attempt.failures++
	if attempt.failures >= unlockFailureLimit {
		attempt.blockedUntil = now.Add(unlockBlockDuration)
	}
	h.unlockAttempts[key] = attempt
}

func (h *Handler) clearUnlockFailures(key string) {
	h.unlockRateMu.Lock()
	delete(h.unlockAttempts, key)
	h.unlockRateMu.Unlock()
}

func (h *Handler) cleanupUnlockAttemptsLocked(now time.Time) {
	for key, attempt := range h.unlockAttempts {
		if now.Sub(attempt.windowStart) >= unlockFailureWindow && !now.Before(attempt.blockedUntil) {
			delete(h.unlockAttempts, key)
		}
	}
}

func (h *Handler) adminConfig(w http.ResponseWriter, r *http.Request) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
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
		Region      string `json:"region"`
		RequestLogs struct {
			RetentionDays int `json:"retention_days"`
		} `json:"request_logs"`
	}
	c.RequestLogs.RetentionDays = 30
	if json.Unmarshal(raw, &c) != nil {
		writeJSON(w, 500, map[string]string{"error": "invalid config"})
		return
	}
	writeJSON(w, 200, c)
}

func (h *Handler) saveAdminConfig(w http.ResponseWriter, r *http.Request) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	var req struct {
		CheckinHours   []int `json:"checkin_hours"`
		KeepaliveHours []int `json:"keepalive_hours"`
		RequestLogs    *struct {
			RetentionDays *int `json:"retention_days"`
		} `json:"request_logs"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	checkinHours, checkinOK := normalizeScheduleHours(req.CheckinHours)
	keepaliveHours, keepaliveOK := normalizeScheduleHours(req.KeepaliveHours)
	if !checkinOK || !keepaliveOK {
		writeJSON(w, 400, map[string]string{"error": "每项至少填写一个 0-23 的整数小时"})
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
	retentionDays := 30
	if existing, ok := doc["request_logs"].(map[string]any); ok {
		if value, ok := existing["retention_days"].(float64); ok {
			retentionDays = int(value)
		}
	}
	if req.RequestLogs != nil && req.RequestLogs.RetentionDays != nil {
		retentionDays = *req.RequestLogs.RetentionDays
	}
	if retentionDays < 0 || retentionDays > 3650 {
		writeJSON(w, 400, map[string]string{"error": "request_logs.retention_days must be between 0 and 3650"})
		return
	}
	schedule := map[string]any{"checkin_hours": checkinHours, "keepalive_hours": keepaliveHours}
	doc["schedule"] = schedule
	requestLogs := map[string]any{"retention_days": retentionDays}
	doc["request_logs"] = requestLogs
	out, _ := json.MarshalIndent(doc, "", "  ")
	if err := writeFileAtomic(h.cfg.ConfigPath, append(out, '\n'), 0600); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	restartRequired := h.cfg.UpdateSchedule == nil
	if h.cfg.UpdateSchedule != nil {
		h.cfg.UpdateSchedule(checkinHours, keepaliveHours)
	}
	if h.cfg.UpdateLogRetention != nil {
		h.cfg.UpdateLogRetention(retentionDays)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "restart_required": restartRequired, "schedule": schedule, "request_logs": requestLogs})
}

func normalizeScheduleHours(hours []int) ([]int, bool) {
	if len(hours) == 0 {
		return nil, false
	}
	seen := make(map[int]struct{}, len(hours))
	out := make([]int, 0, len(hours))
	for _, hour := range hours {
		if hour < 0 || hour > 23 {
			return nil, false
		}
		if _, exists := seen[hour]; exists {
			continue
		}
		seen[hour] = struct{}{}
		out = append(out, hour)
	}
	sort.Ints(out)
	return out, true
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	return writeFileAtomicWith(path, data, mode, os.Rename)
}

// writeFileAtomicWith replaces a regular file atomically. A single-file
// Docker bind mount cannot be replaced with rename from inside the container,
// so fall back to a durable in-place write when the replacement is rejected.
// The fallback keeps mounted config files writable on the host; directory
// mounted auth files continue to use the atomic path.
func writeFileAtomicWith(path string, data []byte, mode os.FileMode, rename func(string, string) error) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".wb2api-config-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := rename(tmpPath, path); err == nil {
		return nil
	} else {
		renameErr := err
		if err := writeFileInPlace(path, data, mode); err != nil {
			return fmt.Errorf("replace %s: %w; in-place fallback: %v", path, renameErr, err)
		}
		return nil
	}
}

func writeFileInPlace(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (h *Handler) runCheckin(w http.ResponseWriter, r *http.Request) {
	if h.cfg.CheckinNow == nil {
		writeJSON(w, 503, map[string]string{"error": "签到服务不可用"})
		return
	}
	go h.cfg.CheckinNow()
	writeJSON(w, 202, map[string]any{"ok": true, "message": "签到任务已启动"})
}

func (h *Handler) refreshCredits(w http.ResponseWriter, r *http.Request) {
	if h.cfg.CreditRefreshNow == nil {
		writeJSON(w, 503, map[string]string{"error": "积分刷新服务不可用"})
		return
	}
	go h.cfg.CreditRefreshNow()
	writeJSON(w, 202, map[string]any{"ok": true, "message": "上游积分刷新已启动"})
}

func normalizeLoginRegion(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "cn", "china":
		return "cn", nil
	case "global", "overseas", "international", "intl":
		return "global", nil
	default:
		return "", fmt.Errorf("登录区域只能选择 cn 或 global")
	}
}

func (h *Handler) loginRegion(r *http.Request) (string, error) {
	if raw := strings.TrimSpace(r.URL.Query().Get("region")); raw != "" {
		return normalizeLoginRegion(raw)
	}
	h.configMu.Lock()
	configured := strings.ToLower(strings.TrimSpace(h.cfg.Region))
	h.configMu.Unlock()
	if configured == "global" {
		return "global", nil
	}
	// all means the pool is mixed, but an OAuth request still needs one
	// concrete upstream host. Keep the historical CN default for API callers
	// that do not send the new query parameter.
	return "cn", nil
}

func (h *Handler) loginCommand(ctx context.Context, arg, region string) ([]byte, error) {
	bin := h.cfg.LoginBin
	if bin == "" {
		bin = "./login"
	}
	cmd := exec.CommandContext(ctx, bin, arg)
	cmd.Dir = filepath.Dir(h.cfg.ConfigPath)
	cmd.Env = setCommandEnv(os.Environ(), "WB2A_LOGIN_REGION", region)
	return cmd.Output()
}

func setCommandEnv(env []string, key, value string) []string {
	prefix := key + "="
	updated := false
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[i] = prefix + value
			updated = true
		}
	}
	if !updated {
		env = append(env, prefix+value)
	}
	return env
}

func (h *Handler) accountURL(w http.ResponseWriter, r *http.Request) {
	region, err := h.loginRegion(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	out, err := h.loginCommand(r.Context(), "url", region)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": fmt.Sprintf("登录初始化失败: %v", err)})
		return
	}
	loginURL := strings.TrimSpace(string(out))
	if loginURL == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "登录初始化未返回授权链接"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": loginURL, "region": region})
}

func (h *Handler) accountPoll(w http.ResponseWriter, r *http.Request) {
	region, err := h.loginRegion(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	out, err := h.loginCommand(r.Context(), "poll", region)
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": "登录尚未完成，请先在浏览器完成授权"})
		return
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Domain       string `json:"domain"`
		Region       string `json:"region"`
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterprise_id"`
		Nickname     string `json:"nickname"`
	}
	if json.Unmarshal(out, &result) != nil || result.AccessToken == "" || result.UID == "" {
		writeJSON(w, 409, map[string]string{"error": "登录尚未完成，请完成授权后重试"})
		return
	}
	if result.Region != "" {
		returnedRegion, regionErr := normalizeLoginRegion(result.Region)
		if regionErr != nil || returnedRegion != region {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "授权区域与请求区域不一致，请重新生成登录链接"})
			return
		}
	}
	if region == "global" && strings.TrimSpace(result.Domain) == "" {
		// Some international OAuth responses omit domain. Persisting the global
		// host is necessary because account.Region() drives every later request.
		result.Domain = "www.workbuddy.ai"
	}
	if filepath.Base(result.UID) != result.UID || strings.ContainsAny(result.UID, `/\`) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "授权返回的 UID 无效"})
		return
	}
	if err := os.MkdirAll(h.cfg.AuthDir, 0700); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	expiresAt := int64(0)
	if result.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + result.ExpiresIn
	}
	doc := map[string]any{"auth": map[string]any{"accessToken": result.AccessToken, "refreshToken": result.RefreshToken, "expiresAt": expiresAt, "domain": result.Domain}, "account": map[string]any{"uid": result.UID, "enterpriseId": result.EnterpriseID, "nickname": result.Nickname}}
	raw, _ := json.MarshalIndent(doc, "", "  ")
	path := filepath.Join(h.cfg.AuthDir, "workbuddy-"+result.UID+".json")
	if err := writeFileAtomic(path, append(raw, '\n'), 0600); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	loadedMixed := false
	if h.cfg.Pool != nil {
		if loaded, loadErr := auth.LoadDir(h.cfg.AuthDir, auth.RegionAll); loadErr == nil {
			regions := make(map[string]struct{}, 2)
			for _, loadedAuth := range loaded {
				regions[loadedAuth.Region()] = struct{}{}
			}
			loadedMixed = len(regions) > 1
			h.cfg.Pool.SyncToDir(loaded)
		} else {
			log.Printf("account poll: reload auths failed: %v", loadErr)
		}
		h.cfg.Pool.Add(&auth.Auth{
			AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, ExpiresAt: expiresAt,
			Domain: result.Domain, UID: result.UID, EnterpriseID: result.EnterpriseID,
			Nickname: result.Nickname, FilePath: path,
		})
	}
	// A newly added account may expose a different regional model catalogue.
	// Force the next /models request to fetch with the expanded pool.
	invalidateDynamicModelsCache()

	response := map[string]any{"ok": true, "uid": result.UID, "nickname": result.Nickname, "region": region}
	var configErr error
	if loadedMixed {
		configErr = h.promoteMixedRegion()
	} else {
		configErr = h.promoteMixedRegionIfNeeded(region)
	}
	if configErr != nil {
		// The account is already usable in the current process. Return a warning
		// so an unwritable config mount does not hide the restart persistence fix.
		response["warning"] = "账号已添加，但混合区域配置未能保存：" + configErr.Error()
	}
	writeJSON(w, http.StatusOK, response)
}

// promoteMixedRegionIfNeeded keeps both CN and global credentials available
// after a user adds an account from the other region. It persists region=all
// for the next restart and updates the running handler after a successful write.
func (h *Handler) promoteMixedRegionIfNeeded(loginRegion string) error {
	h.configMu.Lock()
	configured := strings.ToLower(strings.TrimSpace(h.cfg.Region))
	h.configMu.Unlock()
	if configured == "all" || configured == loginRegion {
		return nil
	}
	return h.promoteMixedRegion()
}

func (h *Handler) promoteMixedRegion() error {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	if h.cfg.ConfigPath == "" {
		h.cfg.Region = "all"
		return nil
	}
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	doc["region"] = "all"
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(h.cfg.ConfigPath, append(out, '\n'), 0600); err != nil {
		// Keep the in-memory region unchanged when persistence fails. The caller
		// already added the account to the live pool, and a later account add can
		// retry this write instead of incorrectly considering it complete.
		return err
	}
	h.cfg.Region = "all"
	return nil
}

func (h *Handler) enableAccount(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "账号 UID 不能为空"})
		return
	}
	if h.cfg.Pool == nil || !h.cfg.Pool.Enable(uid) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
		return
	}
	h.cfg.Pool.Flush()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "message": "账号已启用"})
}

func (h *Handler) disableAccount(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "账号 UID 不能为空"})
		return
	}
	if h.cfg.Pool == nil || h.cfg.Pool.AuthByUID(uid) == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
		return
	}
	h.cfg.Pool.Disable(uid, "manual disabled")
	h.cfg.Pool.Flush()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "message": "账号已禁用"})
}

func (h *Handler) deleteAccount(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "账号 UID 不能为空"})
		return
	}
	if h.cfg.Pool == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
		return
	}
	account := h.cfg.Pool.AuthByUID(uid)
	if account == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
		return
	}
	if removed, busy := h.cfg.Pool.Remove(uid); !removed {
		if busy {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "账号仍有请求处理中，请稍后再删除"})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
		return
	}
	if err := h.removeAccountFile(uid, account); err != nil {
		// 文件删除失败时恢复内存中的账号，避免控制台显示删除成功但账号仍会在下次同步出现。
		h.cfg.Pool.Add(account)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("删除账号文件失败: %v", err)})
		return
	}
	h.cfg.Pool.Flush()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "message": "账号已删除"})
}

func (h *Handler) removeAccountFile(uid string, account *auth.Auth) error {
	snapshot := account.Snapshot()
	path := snapshot.FilePath
	if path == "" {
		if h.cfg.AuthDir == "" || filepath.Base(uid) != uid {
			return nil
		}
		path = filepath.Join(h.cfg.AuthDir, "workbuddy-"+uid+".json")
	}
	if h.cfg.AuthDir == "" {
		return fmt.Errorf("auth_dir 未配置")
	}
	root, err := filepath.Abs(h.cfg.AuthDir)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("账号文件不在 auth_dir 内")
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
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
		"concurrency":     h.cfg.Pool.CapacityForModel(upstream.NormalizeModelID(r.URL.Query().Get("model"))),
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
		"metrics":         h.metricsSnapshot(),
	})
}

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	rangeName := strings.TrimSpace(r.URL.Query().Get("range"))
	if rangeName == "" || rangeName == "all" {
		metrics := h.metricsSnapshot()
		metrics["range"] = "all"
		writeJSON(w, http.StatusOK, metrics)
		return
	}
	durations := map[string]time.Duration{
		"24h": 24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"30d": 30 * 24 * time.Hour,
		"90d": 90 * 24 * time.Hour,
	}
	duration, ok := durations[rangeName]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "invalid_stats_range", "message": "range must be one of 24h, 7d, 30d, 90d, all"}})
		return
	}
	rangeStore, ok := h.cfg.MetricsStore.(RangeMetricsStore)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"code": "range_metrics_unavailable", "message": "time-range metrics are unavailable"}})
		return
	}
	to := time.Now()
	from := to.Add(-duration)
	metrics, err := rangeStore.SnapshotMetricsRange(from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"code": "range_metrics_unavailable", "message": err.Error()}})
		return
	}
	metrics["range"] = rangeName
	metrics["range_start"] = from.Unix()
	metrics["range_end"] = to.Unix()
	metrics["scope"] = "retained_request_logs"
	writeJSON(w, http.StatusOK, metrics)
}

func (h *Handler) requests(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil {
			limit = value
		}
	}
	if h.cfg.RequestLogStore == nil {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []RequestLog{}})
		return
	}
	records, err := h.cfg.RequestLogStore.RecentRequests(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"code": "request_log_unavailable", "message": err.Error()}})
		return
	}
	if records == nil {
		records = []RequestLog{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": records})
}

func (h *Handler) metricsSnapshot() map[string]any {
	if h.cfg.MetricsStore != nil {
		return h.cfg.MetricsStore.SnapshotMetrics()
	}
	return metricsSnapshot()
}

// 静态 WorkBuddy 模型表（动态接口失败时的回退；两种区域共用兼容别名）。
var staticModels = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "kimi-k3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "kimi-k2.7-code", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
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

func invalidateDynamicModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

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
	body, tooLarge, err := readRequestBody(r)
	if tooLarge {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 8 MiB limit")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	chatBody, stream, err := responsesToChat(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var requestDoc map[string]any
	var chatDoc map[string]any
	if json.Unmarshal(body, &requestDoc) != nil || json.Unmarshal(chatBody, &chatDoc) != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	currentMessages, _ := chatDoc["messages"].([]any)
	turnMessages := responseMessages(currentMessages)
	messages := turnMessages
	routeKey := session.ExtractKey(body)
	previousID, _ := requestDoc["previous_response_id"].(string)
	if previousID != "" {
		previousMessages, previousRouteKey, ok := h.loadResponseHistory(previousID)
		if !ok {
			writeOpenAIError(w, http.StatusBadRequest, "previous_response_not_found", "previous_response_id is unknown or expired")
			return
		}
		// Top-level Responses instructions apply to the current request. Keep
		// them before the restored transcript and never insert them between a
		// prior assistant tool call and its function_call_output.
		messages = append(responseInstructionMessages(messages), stripInstructionMessages(previousMessages)...)
		messages = append(messages, responseInputMessages(turnMessages)...)
		routeKey = previousRouteKey
	}
	fallbackResponseID := newResponseID()
	if routeKey == "" {
		routeKey = "responses:" + fallbackResponseID
	}
	chatDoc["messages"] = messages
	meta, _ := chatDoc["metadata"].(map[string]any)
	if meta == nil {
		meta = make(map[string]any)
		chatDoc["metadata"] = meta
	}
	meta["conversation_id"] = routeKey
	chatBody, err = json.Marshal(chatDoc)
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
		responseID := upstream.ResponseID(chat)
		if responseID == "" {
			responseID = fallbackResponseID
		}
		upstream.CopyResponseIDHeaders(w.Header(), rec.Header())
		if w.Header().Get("X-Request-Id") == "" {
			w.Header().Set("X-Request-Id", responseID)
		}
		response := chatToResponse(chat, responseID)
		if assistant := chatAssistantMessage(chat, responseID); assistant != nil {
			h.storeResponse(responseID, append(responseInputMessages(turnMessages), assistant), routeKey, previousID)
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	sw := &responsesStreamWriter{header: make(http.Header), dst: w, fallbackID: fallbackResponseID}
	sw.onComplete = func(responseID string, assistant map[string]any) {
		h.storeResponse(responseID, append(responseInputMessages(turnMessages), assistant), routeKey, previousID)
	}
	h.chatCompletions(sw, chatReq)
}

func responseInstructionMessages(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, 1)
	for _, message := range messages {
		if role, _ := message["role"].(string); role == "system" || role == "developer" {
			out = append(out, message)
		}
	}
	return out
}

func responseInputMessages(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		if role, _ := message["role"].(string); role != "system" && role != "developer" {
			out = append(out, message)
		}
	}
	return out
}

func stripInstructionMessages(messages []map[string]any) []map[string]any {
	return responseInputMessages(messages)
}

func responseMessages(raw []any) []map[string]any {
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if message, ok := item.(map[string]any); ok {
			out = append(out, message)
		}
	}
	return out
}

func newResponseID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		return "resp_" + hex.EncodeToString(b)
	}
	return fmt.Sprintf("resp_%d", time.Now().UnixNano())
}

func (h *Handler) loadResponseHistory(id string) ([]map[string]any, string, bool) {
	seen := make(map[string]struct{})
	var routeKey string
	var turns [][]map[string]any
	for id != "" {
		if _, duplicate := seen[id]; duplicate {
			return nil, "", false
		}
		seen[id] = struct{}{}
		record, ok := h.loadResponseRecord(id)
		if !ok {
			return nil, "", false
		}
		if routeKey == "" {
			routeKey = record.routeKey
		}
		turns = append(turns, record.messages)
		id = record.parentID
	}
	history := make([]map[string]any, 0)
	for i := len(turns) - 1; i >= 0; i-- {
		history = append(history, turns[i]...)
	}
	return history, routeKey, true
}

func (h *Handler) loadResponseRecord(id string) (storedResponse, bool) {
	now := time.Now()
	h.responsesMu.Lock()
	record, ok := h.responseHistory[id]
	if ok && now.After(record.expiresAt) {
		h.deleteResponseLocked(id)
		ok = false
	}
	h.responsesMu.Unlock()
	if ok {
		return record, true
	}
	if h.cfg.ResponseStore == nil {
		return storedResponse{}, false
	}
	raw, ok := h.cfg.ResponseStore.LoadResponse(id)
	if !ok {
		return storedResponse{}, false
	}
	var persisted storedResponseWire
	if json.Unmarshal(raw, &persisted) != nil || len(persisted.Messages) == 0 || persisted.RouteKey == "" {
		return storedResponse{}, false
	}
	record = storedResponse{
		messages: persisted.Messages, parentID: persisted.ParentID, routeKey: persisted.RouteKey,
		expiresAt: now.Add(responseHistoryTTL), size: len(raw),
	}
	h.cacheResponse(id, record)
	return record, true
}

func (h *Handler) storeResponse(id string, messages []map[string]any, routeKey, parentID string) {
	now := time.Now()
	raw, _ := json.Marshal(messages)
	h.cacheResponse(id, storedResponse{
		messages: append([]map[string]any{}, messages...), parentID: parentID, routeKey: routeKey,
		expiresAt: now.Add(responseHistoryTTL), size: len(raw),
	})
	if h.cfg.ResponseStore != nil {
		persisted, err := json.Marshal(storedResponseWire{Messages: messages, ParentID: parentID, RouteKey: routeKey})
		if err == nil {
			h.cfg.ResponseStore.SaveResponse(id, persisted, responseHistoryTTL)
		}
	}
}

func (h *Handler) cacheResponse(id string, record storedResponse) {
	now := time.Now()
	h.responsesMu.Lock()
	defer h.responsesMu.Unlock()
	if _, exists := h.responseHistory[id]; exists {
		h.deleteResponseLocked(id)
	}
	for key, record := range h.responseHistory {
		if now.After(record.expiresAt) {
			h.deleteResponseLocked(key)
		}
	}
	for len(h.responseHistory) >= maxResponseHistory || h.responseBytes+record.size > maxResponseHistoryBytes {
		var oldestID string
		var oldest time.Time
		for key, record := range h.responseHistory {
			if oldestID == "" || record.expiresAt.Before(oldest) {
				oldestID, oldest = key, record.expiresAt
			}
		}
		if oldestID == "" {
			break
		}
		h.deleteResponseLocked(oldestID)
	}
	if record.size > maxResponseHistoryBytes {
		return
	}
	h.responseHistory[id] = record
	h.responseBytes += record.size
}

func (h *Handler) deleteResponseLocked(id string) {
	if record, ok := h.responseHistory[id]; ok {
		h.responseBytes -= record.size
		delete(h.responseHistory, id)
	}
}

func responsesToChat(raw []byte) ([]byte, bool, error) {
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, false, err
	}
	chat := make(map[string]any, len(in))
	for k, v := range in {
		switch k {
		case "input", "instructions", "stream", "max_output_tokens", "conversation", "previous_response_id":
			continue
		}
		chat[k] = v
	}
	delete(chat, "reasoning")
	delete(chat, "text")
	if reasoning, ok := in["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && strings.TrimSpace(effort) != "" {
			chat["reasoning_effort"] = effort
		}
	}
	if text, ok := in["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			if responseFormat := responsesFormatToChat(format); responseFormat != nil {
				chat["response_format"] = responseFormat
			}
		}
	}
	// Responses tools are flat ({type,name,parameters}); Chat Completions
	// expects function metadata nested under `function`.
	if rawTools, ok := in["tools"].([]any); ok {
		tools := make([]any, 0, len(rawTools))
		for _, raw := range rawTools {
			tool, ok := raw.(map[string]any)
			if !ok || tool["type"] != "function" {
				tools = append(tools, raw)
				continue
			}
			if _, nested := tool["function"].(map[string]any); nested {
				tools = append(tools, tool)
				continue
			}
			fn := make(map[string]any, len(tool))
			for k, v := range tool {
				if k != "type" {
					fn[k] = v
				}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		chat["tools"] = tools
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
	// Carry a stable Responses conversation/cache key into the existing chat
	// routing path without sending Responses-only conversation fields upstream.
	if key := session.ExtractKey(raw); key != "" {
		meta, _ := chat["metadata"].(map[string]any)
		if meta == nil {
			meta = make(map[string]any)
			chat["metadata"] = meta
		}
		if _, exists := meta["conversation_id"]; !exists {
			meta["conversation_id"] = key
		}
	}
	stream, _ := in["stream"].(bool)
	chat["stream"] = stream
	if n, ok := in["max_output_tokens"]; ok {
		chat["max_tokens"] = n
	}
	out, err := json.Marshal(chat)
	return out, stream, err
}

func appendResponseInput(msgs *[]map[string]any, input any) {
	appendOne := func(role string, content any) {
		if content != nil && content != "" {
			*msgs = append(*msgs, map[string]any{"role": role, "content": content})
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
			typ, _ := m["type"].(string)
			switch typ {
			case "function_call":
				callID, _ := m["call_id"].(string)
				name, _ := m["name"].(string)
				args, _ := m["arguments"].(string)
				if callID != "" && name != "" {
					call := map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
					if n := len(*msgs); n > 0 && (*msgs)[n-1]["role"] == "assistant" {
						last := (*msgs)[n-1]
						calls, _ := last["tool_calls"].([]any)
						last["tool_calls"] = append(calls, call)
					} else {
						*msgs = append(*msgs, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{call}})
					}
				}
				continue
			case "function_call_output":
				callID, _ := m["call_id"].(string)
				var output any = m["output"]
				if parts, ok := output.([]any); ok {
					output = responseContentToChat(parts)
				}
				if output == nil {
					output = ""
				}
				if callID != "" {
					*msgs = append(*msgs, map[string]any{"role": "tool", "tool_call_id": callID, "content": output})
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
				appendOne(role, responseContentToChat(content))
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

func responseContentToChat(content []any) []any {
	out := make([]any, 0, len(content))
	for _, raw := range content {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := part["type"].(string)
		switch typ {
		case "input_text", "output_text", "text":
			if text, _ := part["text"].(string); text != "" {
				out = append(out, map[string]any{"type": "text", "text": text})
			}
		case "input_image", "image_url":
			if imageURL, ok := part["image_url"].(string); ok && imageURL != "" {
				image := map[string]any{"url": imageURL}
				if detail, _ := part["detail"].(string); detail != "" {
					image["detail"] = detail
				}
				out = append(out, map[string]any{"type": "image_url", "image_url": image})
			} else if imageURL, ok := part["image_url"].(map[string]any); ok {
				out = append(out, map[string]any{"type": "image_url", "image_url": imageURL})
			} else if fileID, _ := part["file_id"].(string); fileID != "" {
				// Some compatible providers accept uploaded image IDs in the
				// image_url object even though the field name is historical.
				out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"file_id": fileID}})
			} else {
				// Preserve malformed image parts so the chat-path validator can
				// return a deterministic invalid_image error instead of silently
				// dropping the content.
				out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{}})
			}
		case "input_audio", "audio":
			audio, _ := part["input_audio"].(map[string]any)
			if audio == nil {
				audio, _ = part["audio"].(map[string]any)
			}
			if audio == nil {
				audio = selectFields(part, "data", "format")
			}
			if len(audio) > 0 {
				out = append(out, map[string]any{"type": "input_audio", "input_audio": audio})
			}
		case "input_file", "file":
			file, _ := part["file"].(map[string]any)
			if file == nil {
				file = selectFields(part, "file_id", "file_data", "filename")
			}
			if len(file) > 0 {
				out = append(out, map[string]any{"type": "file", "file": file})
			}
		}
	}
	return out
}

func selectFields(source map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(keys))
	for _, key := range keys {
		if value, ok := source[key]; ok && value != nil && value != "" {
			out[key] = value
		}
	}
	return out
}

func responsesFormatToChat(format map[string]any) map[string]any {
	typ, _ := format["type"].(string)
	switch typ {
	case "text":
		return map[string]any{"type": "text"}
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		name, _ := format["name"].(string)
		schema := format["schema"]
		if name == "" || schema == nil {
			return nil
		}
		jsonSchema := map[string]any{"name": name, "schema": schema}
		if strict, ok := format["strict"].(bool); ok {
			jsonSchema["strict"] = strict
		}
		return map[string]any{"type": "json_schema", "json_schema": jsonSchema}
	default:
		return nil
	}
}

func chatToResponse(chat map[string]any, id string) map[string]any {
	if id == "" {
		id = newResponseID()
	}
	model, _ := chat["model"].(string)
	text := ""
	finishReason := ""
	var message map[string]any
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			message, _ = choice["message"].(map[string]any)
			text, _ = message["content"].(string)
			finishReason, _ = choice["finish_reason"].(string)
		}
	}

	calls := normalizedAssistantToolCalls(message, id)
	output := make([]any, 0, len(calls)+1)
	if text != "" || len(calls) == 0 {
		output = append(output, map[string]any{
			"id": id + "-item", "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		})
	}
	for i, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		callID, _ := call["id"].(string)
		if callID == "" {
			callID = fmt.Sprintf("call-%s-%d", id, i)
		}
		output = append(output, map[string]any{
			"id": callID, "type": "function_call", "status": "completed", "call_id": callID,
			"name": name, "arguments": args,
		})
	}

	status := "completed"
	out := map[string]any{
		"id": id, "object": "response", "created_at": time.Now().Unix(), "status": status,
		"model": model, "output": output, "output_text": text,
	}
	if finishReason == "length" {
		out["status"] = "incomplete"
		out["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if usage, ok := chat["usage"]; ok {
		out["usage"] = responseUsage(usage)
	}
	return out
}

func chatAssistantMessage(chat map[string]any, responseID string) map[string]any {
	choices, ok := chat["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return nil
	}
	assistant := map[string]any{"role": "assistant"}
	if content, exists := message["content"]; exists {
		assistant["content"] = content
	}
	if calls := normalizedAssistantToolCalls(message, responseID); len(calls) > 0 {
		assistant["tool_calls"] = calls
	}
	if _, hasContent := assistant["content"]; !hasContent {
		if _, hasCalls := assistant["tool_calls"]; !hasCalls {
			return nil
		}
	}
	return assistant
}

func normalizedAssistantToolCalls(message map[string]any, responseID string) []any {
	raw, _ := message["tool_calls"].([]any)
	out := make([]any, 0, len(raw))
	for i, value := range raw {
		call, ok := value.(map[string]any)
		if !ok {
			continue
		}
		copy := make(map[string]any, len(call)+1)
		for k, v := range call {
			copy[k] = v
		}
		if id, _ := copy["id"].(string); id == "" {
			copy["id"] = fmt.Sprintf("call-%s-%d", responseID, i)
		}
		out = append(out, copy)
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
				cached = maxInts(cached, n)
			}
		}
		if details, ok := u["prompt_tokens_details"].(map[string]any); ok {
			if n, ok := numberInt(details["cached_tokens"]); ok {
				cached = maxInts(cached, n)
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
	header      http.Header
	dst         http.ResponseWriter
	buf         bytes.Buffer
	started     bool
	passthrough bool
	completed   bool
	id          string
	fallbackID  string
	model       string
	finish      string
	outputText  strings.Builder
	textStarted bool
	textIndex   int
	nextIndex   int
	usage       map[string]any
	calls       map[int]*responseStreamCall
	onComplete  func(string, map[string]any)
}

type responseStreamCall struct {
	index       int
	id          string
	name        string
	args        strings.Builder
	added       bool
	emittedArgs int
}

func (w *responsesStreamWriter) Header() http.Header { return w.header }
func (w *responsesStreamWriter) WriteHeader(code int) {
	if code >= 400 {
		for k, values := range w.header {
			for _, value := range values {
				w.dst.Header().Add(k, value)
			}
		}
		w.passthrough = true
		w.dst.WriteHeader(code)
	}
}
func (w *responsesStreamWriter) Write(p []byte) (int, error) {
	if w.passthrough {
		return w.dst.Write(p)
	}
	if w.completed {
		return len(p), nil
	}
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
				if err := w.complete(); err != nil {
					return 0, err
				}
				continue
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(payload), &chunk) != nil {
				continue
			}
			if rawErr, ok := chunk["error"]; ok {
				w.ensureID(chunk)
				w.completed = true
				if err := w.emit("response.failed", map[string]any{"type": "response.failed", "response": map[string]any{"id": w.id, "object": "response", "status": "failed", "error": rawErr}}); err != nil {
					return 0, err
				}
				if _, err := io.WriteString(w.dst, "data: [DONE]\n\n"); err != nil {
					return 0, err
				}
				if f, ok := w.dst.(http.Flusher); ok {
					f.Flush()
				}
				return len(p), nil
			}
			if err := w.ensureCreated(chunk); err != nil {
				return 0, err
			}
			if usage, ok := chunk["usage"].(map[string]any); ok {
				w.usage = usage
			}
			if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
				if c, ok := choices[0].(map[string]any); ok {
					if finish, ok := c["finish_reason"].(string); ok && finish != "" {
						w.finish = finish
					}
					if d, ok := c["delta"].(map[string]any); ok {
						if text, ok := d["content"].(string); ok && text != "" {
							if err := w.startTextOutput(); err != nil {
								return 0, err
							}
							w.outputText.WriteString(text)
							if err := w.emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "response_id": w.id, "item_id": w.id + "-item", "output_index": w.textIndex, "content_index": 0, "delta": text}); err != nil {
								return 0, err
							}
						}
						if calls, ok := d["tool_calls"].([]any); ok {
							for _, raw := range calls {
								if call, ok := raw.(map[string]any); ok {
									idx := 0
									if n, ok := numberInt(call["index"]); ok {
										idx = n
									}
									if w.calls == nil {
										w.calls = make(map[int]*responseStreamCall)
									}
									state := w.calls[idx]
									if state == nil {
										state = &responseStreamCall{index: w.nextIndex}
										w.nextIndex++
										w.calls[idx] = state
									}
									fn, _ := call["function"].(map[string]any)
									if callID, _ := call["id"].(string); callID != "" {
										state.id = callID
									}
									name, _ := fn["name"].(string)
									if name != "" {
										state.name = name
									}
									args, _ := fn["arguments"].(string)
									if state.id == "" {
										state.id = fmt.Sprintf("call-%s-%d", w.id, idx)
									}
									if state.name != "" && !state.added {
										state.added = true
										if err := w.emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "response_id": w.id, "output_index": state.index, "item": map[string]any{"id": state.id, "type": "function_call", "status": "in_progress", "call_id": state.id, "name": state.name, "arguments": ""}}); err != nil {
											return 0, err
										}
									}
									if args != "" {
										state.args.WriteString(args)
									}
									if state.added && state.emittedArgs < state.args.Len() {
										allArgs := state.args.String()
										pending := allArgs[state.emittedArgs:]
										state.emittedArgs = len(allArgs)
										if err := w.emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "response_id": w.id, "output_index": state.index, "item_id": state.id, "call_id": state.id, "delta": pending}); err != nil {
											return 0, err
										}
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

func (w *responsesStreamWriter) ensureID(chunk map[string]any) {
	if w.id == "" {
		w.id = upstream.ResponseID(chunk)
	}
	if w.id == "" {
		w.id = upstream.ResponseIDFromHeader(w.header)
	}
	if w.id == "" {
		w.id = w.fallbackID
	}
	if w.id == "" {
		w.id = newResponseID()
	}
	if w.dst.Header().Get("X-Request-Id") == "" {
		w.dst.Header().Set("X-Request-Id", w.id)
	}
	if w.model == "" && chunk != nil {
		w.model, _ = chunk["model"].(string)
	}
}

func (w *responsesStreamWriter) ensureCreated(chunk map[string]any) error {
	w.ensureID(chunk)
	if w.started {
		return nil
	}
	return w.emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": w.id, "object": "response", "status": "in_progress", "model": w.model}})
}

func (w *responsesStreamWriter) complete() error {
	if w.completed {
		return nil
	}
	w.completed = true
	w.ensureID(nil)
	text := w.outputText.String()
	output := make([]any, w.nextIndex)
	if w.textStarted {
		output[w.textIndex] = map[string]any{"id": w.id + "-item", "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
	}
	indexes := make([]int, 0, len(w.calls))
	for index := range w.calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		call := w.calls[index]
		item := map[string]any{"id": call.id, "type": "function_call", "status": "completed", "call_id": call.id, "name": call.name, "arguments": call.args.String()}
		output[call.index] = item
		if err := w.emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "response_id": w.id, "item_id": call.id, "output_index": call.index, "arguments": call.args.String()}); err != nil {
			return err
		}
		if err := w.emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "response_id": w.id, "output_index": call.index, "item": item}); err != nil {
			return err
		}
	}
	status := "completed"
	response := map[string]any{"id": w.id, "object": "response", "status": status, "model": w.model, "output": output, "output_text": text}
	if w.finish == "length" {
		status = "incomplete"
		response["status"] = status
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if w.usage != nil {
		response["usage"] = responseUsage(w.usage)
	}
	if w.onComplete != nil {
		assistant := map[string]any{"role": "assistant", "content": text}
		if len(indexes) > 0 {
			toolCalls := make([]any, 0, len(indexes))
			for _, index := range indexes {
				call := w.calls[index]
				toolCalls = append(toolCalls, map[string]any{"id": call.id, "type": "function", "function": map[string]any{"name": call.name, "arguments": call.args.String()}})
			}
			assistant["tool_calls"] = toolCalls
		}
		w.onComplete(w.id, assistant)
	}
	if w.textStarted {
		if err := w.emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "response_id": w.id, "item_id": w.id + "-item", "output_index": w.textIndex, "content_index": 0, "text": text}); err != nil {
			return err
		}
		if err := w.emit("response.content_part.done", map[string]any{"type": "response.content_part.done", "response_id": w.id, "item_id": w.id + "-item", "output_index": w.textIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}); err != nil {
			return err
		}
		if err := w.emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "response_id": w.id, "output_index": w.textIndex, "item": map[string]any{"id": w.id + "-item", "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}}); err != nil {
			return err
		}
	}
	event := "response.completed"
	if status == "incomplete" {
		event = "response.incomplete"
	}
	if err := w.emit(event, map[string]any{"type": event, "response": response}); err != nil {
		return err
	}
	// A number of OpenAI-compatible clients, including DSH, require the
	// terminal Chat Completions sentinel even when the payload uses Responses
	// lifecycle events. Keep both protocols well formed.
	if _, err := io.WriteString(w.dst, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if f, ok := w.dst.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (w *responsesStreamWriter) startTextOutput() error {
	if w.textStarted {
		return nil
	}
	w.textStarted = true
	w.textIndex = w.nextIndex
	w.nextIndex++
	itemID := w.id + "-item"
	if err := w.emit("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "response_id": w.id, "output_index": w.textIndex,
		"item": map[string]any{"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
	}); err != nil {
		return err
	}
	return w.emit("response.content_part.added", map[string]any{
		"type": "response.content_part.added", "response_id": w.id, "item_id": itemID,
		"output_index": w.textIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (w *responsesStreamWriter) emit(event string, v map[string]any) error {
	raw, _ := json.Marshal(v)
	if !w.started {
		upstream.CopyResponseIDHeaders(w.dst.Header(), w.header)
		w.dst.Header().Set("Content-Type", "text/event-stream")
		w.dst.Header().Set("Cache-Control", "no-cache")
		w.dst.Header().Set("X-Accel-Buffering", "no")
		w.dst.WriteHeader(http.StatusOK)
		w.started = true
	}
	if _, err := fmt.Fprintf(w.dst, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	if f, ok := w.dst.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, tooLarge, err := readRequestBody(r)
	if tooLarge {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 8 MiB limit")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if err := validateImageParts(body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_image", err.Error())
		return
	}
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	passthrough := h.requestPassthrough(r)
	st := newChatStatWithOptions(time.Now(), body, peek.Stream, h.cfg.MetricsStore, h.cfg.RequestLogStore, h.cfg.CreditPolicy, passthrough, r.URL.Path)
	st.completionStore = h.cfg.CompletionStore
	defer st.done()
	// The upstream client canonicalizes public aliases before sending the body.
	// Use the same canonical ID for model-level routing/cooldowns so a limit
	// learned from kimi-k3-1 also applies to the upstream kimi-k3 request.
	routeModel := upstream.NormalizeModelID(st.model)

	tried := map[string]bool{}
	var lastErr error
	var lastKind upstream.ErrKind

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
		// 选号：粘性号优先（同时校验账号级状态和当前模型限流），否则按当前模型轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickAndAcquireByUIDForModel(stickyUID, routeModel)
			if acct == nil {
				// 粘性号当前不可用（冷却/占满）→ 解绑，本次回落普通轮换。
				h.cfg.Session.Unbind(sessKey)
				stickyUID = ""
			}
		}
		if acct == nil {
			acct = h.cfg.Pool.PickAndAcquireForModel(routeModel, tried)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		st.region = acct.Region()
		tried[acct.UID] = true

		// Selection and reservation are atomic; retries are reserved for actual
		// upstream failures rather than races over the last account slot.
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

		rc, status, respBody, upstreamHeaders, terr := h.cfg.Upstream.ChatStreamContextWithHeaders(r.Context(), acct, body)
		upstream.CopyResponseIDHeaders(w.Header(), upstreamHeaders)
		upstreamHeaderID := upstream.ResponseIDFromHeader(upstreamHeaders)
		if upstreamHeaderID != "" {
			st.id = upstreamHeaderID
		}
		if terr != nil {
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 上游 client 已打 transport error 日志。
			st.status = http.StatusServiceUnavailable
			setRequestError(st, "transport_error", terr.Error())
			lastErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			st.status = status
			bodyText := string(respBody)
			kind := upstream.Classify(status, bodyText)
			lastKind = kind
			setRequestError(st, kind.String(), bodyText)
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: bodyText}
			h.applyErrorPolicy(acct.UID, routeModel, kind, bodyText)
			fail(acct.UID)
			continue
		}
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			streamErr := upstream.StreamWithOptionsAndID(w, stats, passthrough, upstreamHeaderID)
			st.ttfb = stats.TTFB()
			st.toks, _ = stats.Tokens()
			st.inputTokens, st.toks, st.totalTokens, st.cacheRead, st.cacheWrite, st.toolCalls = stats.UsageStats()
			if _, ok := stats.Tokens(); !ok {
				st.toks = -1
			}
			if credits, ok := stats.CreditUsage(); ok {
				st.creditsConsumed = credits
				st.creditSource = "upstream"
			}
			if responseID := stats.ResponseID(); responseID != "" && upstreamHeaderID == "" {
				st.id = responseID
			}
			rc.Close()
			if streamErr != nil {
				st.status = http.StatusBadGateway
				setRequestError(st, "upstream_stream_error", streamErr.Error())
				log.Printf("chat_stream model=%s: %v", st.model, streamErr)
				return
			}
			h.cfg.Pool.NoteSuccess(acct.UID)
			if sessKey != "" && h.cfg.Session != nil {
				h.cfg.Session.Bind(sessKey, acct.UID)
			}
			st.status = http.StatusOK
			st.errorCode = ""
			st.errorMessage = ""
			return
		}
		resp, err := upstream.AggregateWithID(rc, upstreamHeaderID)
		rc.Close()
		if err != nil {
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			setRequestError(st, "upstream_parse", err.Error())
			return
		}
		usageStats(resp, st)
		if responseID := upstream.ResponseID(resp); responseID != "" && upstreamHeaderID == "" {
			st.id = responseID
			if w.Header().Get("X-Request-Id") == "" {
				w.Header().Set("X-Request-Id", responseID)
			}
		}
		writeJSON(w, http.StatusOK, resp)
		h.cfg.Pool.NoteSuccess(acct.UID)
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		st.status = http.StatusOK
		st.errorCode = ""
		st.errorMessage = ""
		st.toks = completionTokens(resp)
		return
	}
	// A deterministic upstream 4xx (for example code=11128, which means the
	// first message must be system) is a request error, not an account-pool
	// outage. Preserve that status after rotation so clients can act on the
	// actual cause instead of receiving a misleading 503/no_healthy_account.
	if lastErr != nil && lastKind == upstream.ErrClient {
		if ue, ok := lastErr.(*upstream.Error); ok && ue.Status >= 400 && ue.Status < 500 {
			writeOpenAIError(w, ue.Status, "upstream_client_error", ue.Msg)
			st.status = ue.Status
			setRequestError(st, "upstream_client_error", ue.Msg)
			return
		}
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	st.status = http.StatusServiceUnavailable
	setRequestError(st, "no_healthy_account", msg)
}

// validateImageParts catches malformed multimodal parts before they reach the
// upstream. It intentionally leaves unknown content-part types untouched for
// provider compatibility.
func validateImageParts(body []byte) error {
	var doc struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil // preserve the existing upstream handling for generic JSON errors
	}
	for mi, message := range doc.Messages {
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for pi, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := part["type"].(string)
			if typ != "image_url" && typ != "input_image" {
				continue
			}
			if url, ok := part["image_url"].(string); ok && strings.TrimSpace(url) != "" {
				continue
			}
			if image, ok := part["image_url"].(map[string]any); ok {
				if url, _ := image["url"].(string); strings.TrimSpace(url) != "" {
					continue
				}
				if fileID, _ := image["file_id"].(string); strings.TrimSpace(fileID) != "" {
					continue
				}
			}
			if fileID, _ := part["file_id"].(string); strings.TrimSpace(fileID) != "" {
				continue
			}
			return fmt.Errorf("messages[%d].content[%d] image part requires image_url.url or file_id", mi, pi)
		}
	}
	return nil
}

func setRequestError(st *chatStat, code, message string) {
	st.errorCode = code
	st.errorMessage = truncateRequestError(message)
}

func truncateRequestError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 1024 {
		return message[:1024]
	}
	return message
}

func (h *Handler) requestPassthrough(r *http.Request) bool {
	if !h.cfg.Passthrough {
		return false
	}
	value := strings.TrimSpace(strings.ToLower(r.Header.Get("X-WorkBuddy-Passthrough")))
	if value == "" {
		return true
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify），此处不再按原始 status 二次判断。
// 仅在 chatCompletions 轮转循环内调用：调用方已准备好 lastErr 并打算 continue 换号。
//
// 五条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate：有模型上下文时只冷却该模型；无模型时退回账号级 CoolSoft/CoolRateLimit。
//     上游 code=6004 或带 reset 时间的限流 → 精确冷却到 reset，期间不对该模型兜底重试。
//   - ErrNotFound → Cooldown(CoolSoft)：即时账号级软冷却（404）。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断。
//
// 恢复出口：账号级/模型级 CoolSoft、CoolRateLimit、CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
func (h *Handler) applyErrorPolicy(uid, model string, kind upstream.ErrKind, body string) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		now := time.Now()
		model = strings.TrimSpace(model)
		if model == "-" {
			model = ""
		}
		if isExplicitRateLimit(body) {
			if resetAt, ok := rateLimitResetAt(body, now); ok {
				reason := rateLimitReason(body, resetAt)
				if model != "" {
					h.cfg.Pool.CooldownModelUntil(uid, model, resetAt, reasonWithModel(reason, model))
				} else {
					h.cfg.Pool.CooldownUntil(uid, pool.CoolRateLimit, resetAt, reason)
				}
			} else {
				// code=6004 without a parseable timestamp remains strict: do not
				// keep hammering the account while the upstream window is unknown.
				reason := rateLimitFallbackReason(body)
				if model != "" {
					h.cfg.Pool.CooldownModel(uid, model, h.cfg.SoftCooldown, reasonWithModel(reason, model))
				} else {
					h.cfg.Pool.Cooldown(uid, pool.CoolRateLimit, h.cfg.SoftCooldown, reason)
				}
			}
		} else {
			reason := "429 rate limit"
			if model != "" {
				h.cfg.Pool.CooldownModel(uid, model, h.cfg.SoftCooldown, reasonWithModel(reason, model))
			} else {
				h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, reason)
			}
		}
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

func readRequestBody(r *http.Request) ([]byte, bool, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if len(raw) > maxRequestBodyBytes {
		return nil, true, nil
	}
	return raw, false, err
}

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
