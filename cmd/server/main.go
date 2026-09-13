// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/haozhuma"
	"workbuddy2api/internal/metricsstore"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/smslogin"
	"workbuddy2api/internal/upstream"
)

// buildVersion is injected by Docker builds with -ldflags and can be
// overridden at runtime with WB2A_VERSION for local/dev deployments.
var buildVersion = "dev"

type metricsAdapter struct{ store metricsstore.Backend }

func (m metricsAdapter) AddMetrics(requests, successes, failures, inputTokens, outputTokens, totalTokens, cacheRead, cacheWrite, toolCalls, ttfbMillis, ttfbSamples, latencyMillis, lastRequestUnix int64) error {
	return m.store.Add(metricsstore.Snapshot{Requests: requests, Successes: successes, Failures: failures, InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: totalTokens, CacheRead: cacheRead, CacheWrite: cacheWrite, ToolCalls: toolCalls, TTFBMillis: ttfbMillis, TTFBSamples: ttfbSamples, LatencyMillis: latencyMillis, LastRequestUnix: lastRequestUnix})
}

func (m metricsAdapter) AddCredit(consumed float64, source string) error {
	return m.store.AddCredit(consumed, source)
}

func (m metricsAdapter) RecordRequest(record server.RequestLog) error {
	return m.store.RecordRequest(requestRecord(record))
}

func (m metricsAdapter) RecordCompletion(record server.RequestLog, ttfbObserved bool) error {
	delta := metricsstore.Snapshot{Requests: 1, InputTokens: record.InputTokens, OutputTokens: record.OutputTokens,
		TotalTokens: record.TotalTokens, CacheRead: record.CacheReadTokens, CacheWrite: record.CacheWriteTokens,
		ToolCalls: record.ToolCalls, TTFBMillis: record.TTFBMillis, LatencyMillis: record.LatencyMillis, LastRequestUnix: time.Now().Unix()}
	if record.Status >= 200 && record.Status < 300 {
		delta.Successes = 1
	} else {
		delta.Failures = 1
	}
	if ttfbObserved {
		delta.TTFBSamples = 1
	}
	if record.CreditsConsumed > 0 && record.CreditSource != "unknown" {
		delta.CreditsConsumed = record.CreditsConsumed
		delta.CreditRequests = 1
		if record.CreditSource == "upstream" {
			delta.CreditsUpstream = record.CreditsConsumed
		}
		if record.CreditSource == "estimated" {
			delta.CreditsEstimated = record.CreditsConsumed
		}
	}
	return m.store.RecordCompletion(delta, requestRecord(record))
}

func requestRecord(record server.RequestLog) metricsstore.RequestRecord {
	return metricsstore.RequestRecord{
		ID:                    record.ID,
		CreatedAt:             record.CreatedAt,
		Route:                 record.Route,
		Model:                 record.Model,
		Mode:                  record.Mode,
		Status:                record.Status,
		AccountUID:            record.AccountUID,
		AccountRegion:         record.AccountRegion,
		RequestedOutputTokens: record.RequestedOutputTokens,
		InputTokens:           record.InputTokens,
		OutputTokens:          record.OutputTokens,
		TotalTokens:           record.TotalTokens,
		CacheReadTokens:       record.CacheReadTokens,
		CacheWriteTokens:      record.CacheWriteTokens,
		ToolCalls:             record.ToolCalls,
		TTFBMillis:            record.TTFBMillis,
		LatencyMillis:         record.LatencyMillis,
		CreditsConsumed:       record.CreditsConsumed,
		CreditSource:          record.CreditSource,
		Passthrough:           record.Passthrough,
		ErrorCode:             record.ErrorCode,
		ErrorMessage:          record.ErrorMessage,
	}
}

func (m metricsAdapter) RecentRequests(limit int) ([]server.RequestLog, error) {
	records, err := m.store.RecentRequests(limit)
	if err != nil {
		return nil, err
	}
	out := make([]server.RequestLog, 0, len(records))
	for _, record := range records {
		out = append(out, server.RequestLog{
			ID:                    record.ID,
			CreatedAt:             record.CreatedAt,
			Route:                 record.Route,
			Model:                 record.Model,
			Mode:                  record.Mode,
			Status:                record.Status,
			AccountUID:            record.AccountUID,
			AccountRegion:         record.AccountRegion,
			RequestedOutputTokens: record.RequestedOutputTokens,
			InputTokens:           record.InputTokens,
			OutputTokens:          record.OutputTokens,
			TotalTokens:           record.TotalTokens,
			CacheReadTokens:       record.CacheReadTokens,
			CacheWriteTokens:      record.CacheWriteTokens,
			ToolCalls:             record.ToolCalls,
			TTFBMillis:            record.TTFBMillis,
			LatencyMillis:         record.LatencyMillis,
			CreditsConsumed:       record.CreditsConsumed,
			CreditSource:          record.CreditSource,
			Passthrough:           record.Passthrough,
			ErrorCode:             record.ErrorCode,
			ErrorMessage:          record.ErrorMessage,
		})
	}
	return out, nil
}

func (m metricsAdapter) SnapshotMetrics() map[string]any {
	snapshot, err := m.store.Snapshot()
	if err != nil {
		return map[string]any{"error": "metrics unavailable"}
	}
	return metricsSnapshotMap(snapshot)
}

func (m metricsAdapter) SnapshotMetricsRange(from, to time.Time) (map[string]any, error) {
	store, ok := m.store.(metricsstore.RangeSnapshotter)
	if !ok {
		return nil, fmt.Errorf("metrics backend does not support time ranges")
	}
	snapshot, err := store.SnapshotRange(from, to)
	if err != nil {
		return nil, err
	}
	return metricsSnapshotMap(snapshot), nil
}

func metricsSnapshotMap(snapshot metricsstore.Snapshot) map[string]any {
	requests := snapshot.Requests
	ttfbSamples := snapshot.TTFBSamples
	avgLatency := int64(0)
	if requests > 0 {
		avgLatency = snapshot.LatencyMillis / requests
	}
	avgTTFB := int64(0)
	if ttfbSamples > 0 {
		avgTTFB = snapshot.TTFBMillis / ttfbSamples
	}
	uncachedInput := snapshot.InputTokens - snapshot.CacheRead
	if uncachedInput < 0 {
		uncachedInput = 0
	}
	cacheHitRate := float64(0)
	if snapshot.InputTokens > 0 {
		cacheHitRate = float64(snapshot.CacheRead) / float64(snapshot.InputTokens) * 100
	}
	return map[string]any{"requests": requests, "successes": snapshot.Successes, "failures": snapshot.Failures, "input_tokens": snapshot.InputTokens, "uncached_input_tokens": uncachedInput, "output_tokens": snapshot.OutputTokens, "total_tokens": snapshot.TotalTokens, "cache_read_tokens": snapshot.CacheRead, "cache_write_tokens": snapshot.CacheWrite, "cache_hit_rate": cacheHitRate, "tool_calls": snapshot.ToolCalls, "credits_consumed": snapshot.CreditsConsumed, "credits_upstream": snapshot.CreditsUpstream, "credits_estimated": snapshot.CreditsEstimated, "credit_requests": snapshot.CreditRequests, "avg_ttfb_ms": avgTTFB, "avg_latency_ms": avgLatency, "last_request_at": snapshot.LastRequestUnix}
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
			if err == nil && cfg.APIKey == "" {
				log.Fatalf("config %s is missing and WB2A_API_KEY is not set; refusing to start without API authentication", *cfgPath)
			}
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir, cfg.Region)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d %s account(s) from %s", len(auths), cfg.Region, cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)
	var metricsDB metricsstore.Backend
	if cfg.Postgres.DSN != "" {
		metricsDB, err = metricsstore.OpenPostgres(cfg.Postgres.DSN, cfg.Postgres.MaxOpenConns, cfg.Postgres.MaxIdleConns, cfg.PostgresMaxLifetime, cfg.PostgresMaxIdleTime)
		if err != nil && cfg.Postgres.FallbackToSQLite {
			log.Printf("metrics postgres unavailable: %v; falling back to sqlite", err)
			metricsDB, err = metricsstore.Open(filepath.Join(filepath.Dir(cfg.StateFile), "metrics.db"))
		}
		if err != nil {
			log.Printf("metrics postgres unavailable: %v; using in-memory metrics", err)
		}
	} else {
		metricsDB, err = metricsstore.Open(filepath.Join(filepath.Dir(cfg.StateFile), "metrics.db"))
		if err != nil {
			log.Printf("metrics sqlite unavailable: %v; using in-memory metrics", err)
		}
	}
	if metricsDB != nil {
		defer metricsDB.Close()
	}
	var persistentMetrics server.MetricsStore
	var requestLogs server.RequestLogStore
	var completions server.CompletionStore
	if metricsDB != nil {
		adapter := metricsAdapter{store: metricsDB}
		persistentMetrics = adapter
		requestLogs = adapter
		completions = adapter
	}
	var responseStore server.ResponseStore
	if persistedResponses, ok := store.(server.ResponseStore); ok {
		responseStore = persistedResponses
	}

	p := pool.New(cfg.StateFile)
	defer p.Flush() // 进程退出前强制落盘（后台 flush 每 5s 一次，退出时补一次）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		RequestCredits: metricsDB,
		CheckinHours:   cfg.Schedule.CheckinHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
	})

	// SMSLogin 与 AutoEnroll 必须共享同一个管理器：代理池冷却是全局状态，
	// 各建一份会让手动发码和自动加号在 30 分钟内撞同一个出口 IP。
	smsManager := newSMSLoginManager(cfg)

	h := server.NewHandler(server.Config{
		Pool:             p,
		Upstream:         up,
		APIKey:           cfg.APIKey,
		FrontendPassword: cfg.FrontendPassword,
		ConfigPath:       *cfgPath,
		AuthDir:          cfg.AuthDir,
		Region:           cfg.Region,
		LoginBin:         "/app/login",
		CheckinNow:       sch.RunCheckinNow,
		CreditRefreshNow: sch.RunCreditRefreshNow,
		CheckinAccount:   sch.CheckinAccount,
		KeepaliveAccount: sch.KeepaliveAccount,
		// 短信直登只走中国区 codebuddy.cn 的 OneID/Keycloak；海外版继续用 OAuth 链接。
		SMSLogin: smsManager,
		// 豪猪自动加号（可选）：取号→直登→落盘全自动。与手动发码共用代理池；
		// AutoEnroll 在 NewHandler 内部组装（persist 回调指向 handler）。
		HaozhumaClient:  newHaozhumaClient(cfg),
		HaozhumaSid:     strings.TrimSpace(cfg.SMS.Haozhuma.Sid),
		UpdateSchedule:  sch.UpdateSchedule,
		Session:         sessRouter,
		StickyCount:     sessCount,
		RedisMode:       redisMode,
		ResponseStore:   responseStore,
		MetricsStore:    persistentMetrics,
		RequestLogStore: requestLogs,
		CompletionStore: completions,
		CreditPolicy: server.CreditPolicy{
			InputPer1K:       cfg.Billing.InputCreditsPer1KTokens,
			OutputPer1K:      cfg.Billing.OutputCreditsPer1KTokens,
			CachedInputPer1K: cfg.Billing.CachedInputCreditsPer1KTokens,
		},
		Passthrough:   cfg.Features.Passthrough,
		Version:       runtimeVersion(),
		UpdateCommand: os.Getenv("WB2A_UPDATE_COMMAND"),
		SoftCooldown:  cfg.SoftRateDur,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.RunCreditRefreshNow()
	go sch.RunRequestCreditRefreshNow()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// Bound header memory usage for internet-facing deployments. Request
		// bodies are limited by the handlers before they reach the upstream.
		MaxHeaderBytes: 32 << 10,
		// Close idle keep-alive connections periodically without affecting
		// active streaming responses.
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("workbuddy2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

func runtimeVersion() string {
	if value := strings.TrimSpace(os.Getenv("WB2A_VERSION")); value != "" {
		return value
	}
	return buildVersion
}

// newSMSLoginManager 组装短信直登管理器。
//
// 未配置 sms.two_captcha_key 时不装 solver：遇到 need_captcha 会保持原有行为
// （提示改用浏览器授权），而不是因为缺密钥就整体不可用。
func newSMSLoginManager(cfg *Config) *smslogin.Manager {
	m := smslogin.NewManager(smslogin.DefaultEndpoints(), 10*time.Minute)
	if solver := smslogin.NewTwoCaptchaSolver(cfg.SMS.TwoCaptchaKey); solver != nil {
		m.SetSolver(solver)
		log.Printf("sms login: 2captcha enabled for human verification challenges")
	}
	// 登录代理只影响短信直登链路，号池的日常 API 调用不走它。
	// file 优先：Webshare 这类静态名单每次登录换一条；url 是单出口（1024proxy 粘性或一条静态）。
	if file := strings.TrimSpace(cfg.SMS.Proxy.File); file != "" {
		pool, err := smslogin.LoadPoolDialer(file, cfg.SMSProxyCooldownDur)
		if err != nil {
			log.Fatalf("sms login: 加载代理名单 %s: %v", file, err)
		}
		m.SetProxyDialer(pool)
		log.Printf("sms login: proxy pool enabled (%d endpoints, cooldown=%s)", pool.Len(), cfg.SMSProxyCooldownDur)
	} else if url := strings.TrimSpace(cfg.SMS.Proxy.URL); url != "" {
		d := smslogin.NewResolverProxyDialerWithRegion(url, cfg.SMS.Proxy.Region, cfg.SMS.Proxy.StickyMinutes)
		d.InjectSID = cfg.SMS.Proxy.InjectSID
		m.SetProxyDialer(d)
		log.Printf("sms login: outbound proxy enabled (inject_sid=%v region=%s sticky=%dm)",
			d.InjectSID, strings.ToUpper(strings.TrimSpace(cfg.SMS.Proxy.Region)), stickyMinutesOrDefault(cfg.SMS.Proxy.StickyMinutes))
	}
	return m
}

func stickyMinutesOrDefault(v int) int {
	if v > 0 {
		return v
	}
	return 30
}

// newHaozhumaClient 建豪猪客户端。未配置账号或项目 ID 时返回 nil（端点关闭）。
// persist/find 回调由 server.NewHandler 内部注入（依赖 handler 自身状态）。
//
// 有 user/pass 时优先用它们 login（token 失效能自动重登）；只有 token
// 时用 NewWithCredentials 尽量带上账密，便于运行期重登。
func newHaozhumaClient(cfg *Config) *haozhuma.Client {
	hz := cfg.SMS.Haozhuma
	sid := strings.TrimSpace(hz.Sid)
	if sid == "" {
		return nil
	}
	user := strings.TrimSpace(hz.User)
	pass := hz.Pass
	token := strings.TrimSpace(hz.Token)
	author := strings.TrimSpace(hz.Author)
	uid := strings.TrimSpace(hz.UID)
	isp := strings.TrimSpace(hz.ISP)

	setup := func(c *haozhuma.Client) *haozhuma.Client {
		c.Author, c.UID, c.ISP = author, uid, isp
		return c
	}

	if user != "" && pass != "" {
		c, err := haozhuma.Login(user, pass)
		if err != nil {
			// login 失败但手里有 token 时仍可先跑（token 可能还有效）。
			if token != "" {
				log.Printf("auto-enroll: 豪猪 login 失败(%v)，改用已配置 token", err)
				return setup(haozhuma.NewWithCredentials(token, user, pass))
			}
			log.Printf("auto-enroll: 豪猪登录失败，自动加号关闭: %v", err)
			return nil
		}
		log.Printf("auto-enroll: haozhuma login ok (sid=%s uid=%q isp=%q author=%q)", sid, uid, isp, author)
		return setup(c)
	}
	if token != "" {
		log.Printf("auto-enroll: haozhuma token configured (sid=%s uid=%q isp=%q author=%q)", sid, uid, isp, author)
		return setup(haozhuma.New(token))
	}
	return nil
}
