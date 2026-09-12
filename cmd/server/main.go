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
	"syscall"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/metricsstore"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

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
		Pool:                    p,
		Upstream:                up,
		RequestCredits:          metricsDB,
		CheckinHours:            cfg.Schedule.CheckinHours,
		KeepaliveHours:          cfg.Schedule.KeepaliveHours,
		RequestLogRetentionDays: cfg.RequestLogs.RetentionDays,
	})

	h := server.NewHandler(server.Config{
		Pool:               p,
		Upstream:           up,
		APIKey:             cfg.APIKey,
		FrontendPassword:   cfg.FrontendPassword,
		ConfigPath:         *cfgPath,
		AuthDir:            cfg.AuthDir,
		Region:             cfg.Region,
		LoginBin:           "/app/login",
		CheckinNow:         sch.RunCheckinNow,
		CreditRefreshNow:   sch.RunCreditRefreshNow,
		UpdateSchedule:     sch.UpdateSchedule,
		UpdateLogRetention: sch.UpdateRequestLogRetention,
		Session:            sessRouter,
		StickyCount:        sessCount,
		RedisMode:          redisMode,
		ResponseStore:      responseStore,
		MetricsStore:       persistentMetrics,
		RequestLogStore:    requestLogs,
		CompletionStore:    completions,
		CreditPolicy: server.CreditPolicy{
			InputPer1K:       cfg.Billing.InputCreditsPer1KTokens,
			OutputPer1K:      cfg.Billing.OutputCreditsPer1KTokens,
			CachedInputPer1K: cfg.Billing.CachedInputCreditsPer1KTokens,
		},
		Passthrough:  cfg.Features.Passthrough,
		SoftCooldown: cfg.SoftRateDur,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.RunCreditRefreshNow()
	go sch.RunRequestCreditRefreshNow()
	go sch.RunRequestLogCleanupNow()
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
