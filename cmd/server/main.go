// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
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

type metricsAdapter struct{ store *metricsstore.Store }

func (m metricsAdapter) AddMetrics(requests, successes, failures, inputTokens, outputTokens, totalTokens, cacheRead, cacheWrite, toolCalls, ttfbMillis, ttfbSamples, latencyMillis, lastRequestUnix int64) error {
	return m.store.Add(metricsstore.Snapshot{Requests: requests, Successes: successes, Failures: failures, InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: totalTokens, CacheRead: cacheRead, CacheWrite: cacheWrite, ToolCalls: toolCalls, TTFBMillis: ttfbMillis, TTFBSamples: ttfbSamples, LatencyMillis: latencyMillis, LastRequestUnix: lastRequestUnix})
}

func (m metricsAdapter) SnapshotMetrics() map[string]any {
	snapshot, err := m.store.Snapshot()
	if err != nil {
		return map[string]any{"error": "metrics unavailable"}
	}
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
	return map[string]any{"requests": requests, "successes": snapshot.Successes, "failures": snapshot.Failures, "input_tokens": snapshot.InputTokens, "output_tokens": snapshot.OutputTokens, "total_tokens": snapshot.TotalTokens, "cache_read_tokens": snapshot.CacheRead, "cache_write_tokens": snapshot.CacheWrite, "tool_calls": snapshot.ToolCalls, "avg_ttfb_ms": avgTTFB, "avg_latency_ms": avgLatency, "last_request_at": snapshot.LastRequestUnix}
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
	metricsDB, err := metricsstore.Open(filepath.Join(filepath.Dir(cfg.StateFile), "metrics.db"))
	if err != nil {
		log.Printf("metrics sqlite unavailable: %v; using in-memory metrics", err)
	}
	if metricsDB != nil {
		defer metricsDB.Close()
	}
	var persistentMetrics server.MetricsStore
	if metricsDB != nil {
		persistentMetrics = metricsAdapter{store: metricsDB}
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
		CheckinHours:   cfg.Schedule.CheckinHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
	})

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
		UpdateSchedule:   sch.UpdateSchedule,
		Session:          sessRouter,
		StickyCount:      sessCount,
		RedisMode:        redisMode,
		ResponseStore:    responseStore,
		MetricsStore:     persistentMetrics,
		SoftCooldown:     cfg.SoftRateDur,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
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
