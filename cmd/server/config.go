// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 顶层配置。
type Config struct {
	Listen           string `json:"listen"`            // ":7863"
	APIKey           string `json:"api_key"`           // 空 = 不鉴权
	FrontendPassword string `json:"frontend_password"` // 前端控制台密码；空 = 不启用
	AuthDir          string `json:"auth_dir"`          // ./auths
	StateFile        string `json:"state_file"`        // ./data/state.json
	Region           string `json:"region"`            // "cn" / "global" / "all"

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "60s"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours   []int `json:"checkin_hours"`   // [9,21]
		KeepaliveHours []int `json:"keepalive_hours"` // [22]
	} `json:"schedule"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"` // 默认 120
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
		// Passthrough enables raw upstream SSE forwarding for streaming chat completions.
		Passthrough bool `json:"passthrough"`
	} `json:"features"`

	Billing struct {
		// Values are credits per 1,000 tokens. Zero leaves unknown usage unestimated.
		InputCreditsPer1KTokens       float64 `json:"input_credits_per_1k_tokens"`
		OutputCreditsPer1KTokens      float64 `json:"output_credits_per_1k_tokens"`
		CachedInputCreditsPer1KTokens float64 `json:"cached_input_credits_per_1k_tokens"`
	} `json:"billing"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	SMS struct {
		// TwoCaptchaKey 是 2captcha 的 clientKey。仅在中国区短信直登遇到
		// need_captcha 时使用；留空则该场景提示改用浏览器授权，不影响其他流程。
		TwoCaptchaKey string `json:"two_captcha_key"`
		// Proxy 把整条短信登录链路放到住宅代理后面：同一 IP 半小时内只能注册
		// 一个号，多次注册会被上游风控。每次登录换一个随机粘性会话（sid），
		// 天然拿到不同出口 IP；号池的日常 API 调用不走这个代理。
		Proxy struct {
			// URL 形如 http://用户名:密码@网关:端口。与 file 二选一；file 优先。
			URL string `json:"url"`
			// File 代理名单路径（每行 host:port:user:pass 或完整 URL）。
			// 每次登录换一条，用过的条目进入 cooldown。
			File string `json:"file"`
			// Cooldown 同一条代理再次用于注册的最短间隔，默认 "30m"。
			Cooldown string `json:"cooldown"`
			// Region 可选 ISO 3166-1 两位码（如 HK）。仅在 inject_sid 时拼进用户名。
			Region string `json:"region"`
			// InjectSID 为 true 时按 1024proxy 约定改写用户名（每次登录换 sticky IP）。
			// 普通静态代理必须为 false，否则账密会被改坏。
			InjectSID bool `json:"inject_sid"`
			// StickyMinutes 粘性时长 1-120，默认 30。一次登录十几个请求必须
			// 落在同一个 IP 上，时长要盖过登录全程。
			StickyMinutes int `json:"sticky_minutes"`
		} `json:"proxy"`
		// Haozhuma 豪猪接码平台（haozhuma.com），用于自动加号：取号 →
		// 走本服务的短信直登链 → 收码 → 落盘账号。
		Haozhuma struct {
			// User / Pass 豪猪 API 账号密码（login 换 token 用）。
			User string `json:"user"`
			Pass string `json:"pass"`
			// Token 已有 token 时直接用，跳过 login。
			Token string `json:"token"`
			// Sid 项目 ID。52283 = 腾讯科技[限对接]。
			Sid string `json:"sid"`
		} `json:"haozhuma"`
	} `json:"sms"`

	Postgres struct {
		DSN              string `json:"dsn"`
		MaxOpenConns     int    `json:"max_open_conns"`
		MaxIdleConns     int    `json:"max_idle_conns"`
		ConnMaxLifetime  string `json:"conn_max_lifetime"`
		ConnMaxIdleTime  string `json:"conn_max_idle_time"`
		FallbackToSQLite bool   `json:"fallback_to_sqlite"`
	} `json:"postgres"`

	Pool struct {
		MaxInFlight        int     `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		BreakerThreshold   int     `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string  `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string  `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	// 解析后
	SoftRateDur         time.Duration `json:"-"`
	BreakerCooldownDur  time.Duration `json:"-"`
	BreakerCooldownMaxD time.Duration `json:"-"`
	SessionTTL          time.Duration `json:"-"`
	SessionGCInterval   time.Duration `json:"-"`
	SMSProxyCooldownDur time.Duration `json:"-"`
	PostgresMaxLifetime time.Duration `json:"-"`
	PostgresMaxIdleTime time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
		Region:    "cn",
	}
	c.Cooldown.SoftRate = "60s"
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.KeepaliveHours = []int{22}
	c.Upstream.TimeoutSeconds = 120
	c.Features.SanitizeBlacklistFingerprints = true
	c.Features.Passthrough = false
	c.Pool.MaxInFlight = 3
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	c.Postgres.MaxOpenConns = 16
	c.Postgres.MaxIdleConns = 8
	c.Postgres.ConnMaxLifetime = "30m"
	c.Postgres.ConnMaxIdleTime = "5m"
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_FRONTEND_PASSWORD"); v != "" {
		c.FrontendPassword = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_REGION"); v != "" {
		c.Region = v
	}
	if v := os.Getenv("WB2A_POSTGRES_DSN"); v != "" {
		c.Postgres.DSN = v
	}
	if v := os.Getenv("WB2A_POSTGRES_MAX_OPEN_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Postgres.MaxOpenConns = n
		}
	}
	if v := os.Getenv("WB2A_POSTGRES_MAX_IDLE_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Postgres.MaxIdleConns = n
		}
	}
	if v := os.Getenv("WB2A_POSTGRES_CONN_MAX_LIFETIME"); v != "" {
		c.Postgres.ConnMaxLifetime = v
	}
	if v := os.Getenv("WB2A_POSTGRES_CONN_MAX_IDLE_TIME"); v != "" {
		c.Postgres.ConnMaxIdleTime = v
	}
	if v := os.Getenv("WB2A_POSTGRES_FALLBACK_SQLITE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Postgres.FallbackToSQLite = b
		}
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_PASSTHROUGH"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.Passthrough = b
		}
	}
	if v := os.Getenv("WB2A_INPUT_CREDITS_PER_1K"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			c.Billing.InputCreditsPer1KTokens = n
		}
	}
	if v := os.Getenv("WB2A_OUTPUT_CREDITS_PER_1K"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			c.Billing.OutputCreditsPer1KTokens = n
		}
	}
	if v := os.Getenv("WB2A_CACHED_INPUT_CREDITS_PER_1K"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			c.Billing.CachedInputCreditsPer1KTokens = n
		}
	}
}

func (c *Config) normalize() error {
	var err error
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	if strings.TrimSpace(c.SMS.Proxy.Cooldown) == "" {
		c.SMS.Proxy.Cooldown = "30m"
	}
	if c.SMSProxyCooldownDur, err = time.ParseDuration(c.SMS.Proxy.Cooldown); err != nil {
		return fmt.Errorf("sms.proxy.cooldown: %w", err)
	}
	if c.Postgres.ConnMaxLifetime == "" {
		c.Postgres.ConnMaxLifetime = "30m"
	}
	if c.Postgres.ConnMaxIdleTime == "" {
		c.Postgres.ConnMaxIdleTime = "5m"
	}
	if c.PostgresMaxLifetime, err = time.ParseDuration(c.Postgres.ConnMaxLifetime); err != nil {
		return fmt.Errorf("postgres.conn_max_lifetime: %w", err)
	}
	if c.PostgresMaxIdleTime, err = time.ParseDuration(c.Postgres.ConnMaxIdleTime); err != nil {
		return fmt.Errorf("postgres.conn_max_idle_time: %w", err)
	}
	if c.Postgres.MaxOpenConns <= 0 {
		c.Postgres.MaxOpenConns = 16
	}
	if c.Postgres.MaxIdleConns <= 0 || c.Postgres.MaxIdleConns > c.Postgres.MaxOpenConns {
		c.Postgres.MaxIdleConns = c.Postgres.MaxOpenConns / 2
		if c.Postgres.MaxIdleConns < 1 {
			c.Postgres.MaxIdleConns = 1
		}
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	c.Billing.InputCreditsPer1KTokens = validCreditRate(c.Billing.InputCreditsPer1KTokens)
	c.Billing.OutputCreditsPer1KTokens = validCreditRate(c.Billing.OutputCreditsPer1KTokens)
	c.Billing.CachedInputCreditsPer1KTokens = validCreditRate(c.Billing.CachedInputCreditsPer1KTokens)
	c.Region = strings.ToLower(strings.TrimSpace(c.Region))
	if c.Region == "" {
		c.Region = "cn"
	}
	if c.Region == "mixed" {
		c.Region = "all"
	}
	if c.Region != "cn" && c.Region != "global" && c.Region != "all" {
		return fmt.Errorf("region must be cn, global, or all, got %q", c.Region)
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	return nil
}

func validCreditRate(value float64) float64 {
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}
