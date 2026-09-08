// Package redisstore 封装 Upstash（Redis）持久化，并提供内存降级（Noop）。
//
// 设计约束：Upstash 走公网 TLS，单次 RTT 可能 50~300ms，因此所有写操作都是
// fire-and-forget（后台 goroutine + 失败仅 debug 日志），读操作只发生在启动时
// （加载粘性会话镜像、恢复冷却/熔断快照）。内存为主、Redis 为辅。
//
// 未配置 url / 连接失败时降级为 Noop：一切功能照常工作（纯内存模式），
// 上层只打一条启动警告日志。
package redisstore

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyTTL 粘性会话镜像 + 状态快照的默认 TTL（redis 侧兜底，防脏数据长期滞留）。
const keyTTL = 7 * 24 * time.Hour

// Store 只放本期需要的方法。上下文由实现内部构造（读操作配短超时，写操作 fire-and-forget）。
type Store interface {
	// SetBind 异步镜像粘性会话绑定（key→uid），带 TTL。
	SetBind(key, uid string, ttl time.Duration)
	// DelBind 异步删除粘性会话绑定。
	DelBind(key string)
	// LoadBinds 全量读取粘性会话绑定（key→uid，key 已剥前缀）；仅在启动时调用（同步）。
	// 供冷启动恢复粘性映射（防重启丢粘性）。
	LoadBinds() map[string]string
	// SaveState 异步写池状态 JSON 快照（与本地 state.json 并存，仅作恢复备份）。
	SaveState(data []byte)
	// LoadState 读池状态快照；仅在启动时调用（同步）。
	LoadState() ([]byte, bool)
}

const (
	bindPrefix     = "wb2api:bind:"
	responsePrefix = "wb2api:response:"
	stateKey       = "wb2api:state"
	readTimeout    = 3 * time.Second
)

// New 根据 url+token 构建 Store。
//   - url 为空 → Noop（纯内存模式）
//   - url 已是完整 rediss:// URL 则直接 ParseURL；否则用 token 组装 rediss://default:token@host:6379
//   - Ping 失败 → Noop + 启动警告（硬性降级要求：不因 Redis 不可用而失败）
func New(url, token string) Store {
	if url == "" {
		log.Printf("[redisstore] upstash 未配置，进入纯内存模式（Noop 降级）")
		return Noop{}
	}

	full := normalizeURL(url, token)
	opt, err := redis.ParseURL(full)
	if err != nil {
		log.Printf("[redisstore] 警告: redis 连接串解析失败 (%v)，降级 Noop", err)
		return Noop{}
	}
	opt.ReadTimeout = readTimeout
	opt.WriteTimeout = readTimeout
	client := redis.NewClient(opt)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		log.Printf("[redisstore] 警告: upstash 连接失败 (%v)，降级 Noop（纯内存模式）", err)
		_ = client.Close()
		return Noop{}
	}
	log.Printf("[redisstore] upstash 已连接 (addr=%s)", opt.Addr)
	return &Upstash{client: client, versions: make(map[string]uint64)}
}

// normalizeURL 把 url+token 归一化为可直接 ParseURL 的完整 rediss:// URL。
// 若 url 本身已含 scheme（rediss://、redis://、https://...upstash.io 等）：
//   - rediss:// 或 redis:// 原样返回（已是完整连接串）
//   - 其余（如 https://xxx.upstash.io）剥掉 "://" 前缀只取 host，再按
//     "rediss://default:<token>@<host>:6379" 组装
func normalizeURL(url, token string) string {
	if len(url) >= 8 && (url[:8] == "rediss:/" || url[:7] == "redis:/") {
		return url
	}
	host := url
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	return "rediss://default:" + token + "@" + host + ":6379"
}

// Upstash 真实现：redis.Client 封装。
type Upstash struct {
	client    *redis.Client
	writeMu   sync.Mutex
	versionMu sync.Mutex
	versions  map[string]uint64
}

func bindKey(key string) string { return bindPrefix + key }

// SetBind 异步镜像粘性会话绑定。
func (u *Upstash) SetBind(key, uid string, ttl time.Duration) {
	if ttl <= 0 {
		ttl = keyTTL
	}
	u.scheduleWrite(bindKey(key), "SetBind "+key, func(ctx context.Context) error {
		return u.client.Set(ctx, bindKey(key), uid, ttl).Err()
	})
}

// DelBind 异步删除粘性会话绑定。
func (u *Upstash) DelBind(key string) {
	u.scheduleWrite(bindKey(key), "DelBind "+key, func(ctx context.Context) error {
		return u.client.Del(ctx, bindKey(key)).Err()
	})
}

// SaveState 异步写池状态 JSON 快照。
func (u *Upstash) SaveState(data []byte) {
	snapshot := append([]byte(nil), data...)
	u.scheduleWrite(stateKey, "SaveState", func(ctx context.Context) error {
		return u.client.Set(ctx, stateKey, snapshot, keyTTL).Err()
	})
}

// SaveResponse persists one Responses API turn for previous_response_id
// continuation across process restarts and multiple replicas.
func (u *Upstash) SaveResponse(id string, data []byte, ttl time.Duration) {
	if ttl <= 0 {
		ttl = keyTTL
	}
	snapshot := append([]byte(nil), data...)
	key := responsePrefix + id
	u.scheduleWrite(key, "SaveResponse "+id, func(ctx context.Context) error {
		return u.client.Set(ctx, key, snapshot, ttl).Err()
	})
}

// LoadResponse reads one persisted Responses API turn.
func (u *Upstash) LoadResponse(id string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	value, err := u.client.Get(ctx, responsePrefix+id).Bytes()
	return value, err == nil
}

// scheduleWrite serializes remote writes and drops an operation when a newer
// write for the same Redis key was queued before it started. This preserves
// call order without adding network latency to request handlers.
func (u *Upstash) scheduleWrite(key, label string, write func(context.Context) error) {
	u.versionMu.Lock()
	if u.versions == nil {
		u.versions = make(map[string]uint64)
	}
	u.versions[key]++
	version := u.versions[key]
	u.versionMu.Unlock()
	go func() {
		u.writeMu.Lock()
		defer u.writeMu.Unlock()
		u.versionMu.Lock()
		latest := u.versions[key]
		u.versionMu.Unlock()
		if version != latest {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := write(ctx)
		u.versionMu.Lock()
		if u.versions[key] == version {
			delete(u.versions, key)
		}
		u.versionMu.Unlock()
		if err != nil {
			log.Printf("[redisstore] debug: %s: %v", label, err)
		}
	}()
}

// LoadState 同步读池状态快照。
func (u *Upstash) LoadState() ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	v, err := u.client.Get(ctx, stateKey).Bytes()
	if err != nil {
		return nil, false
	}
	return v, true
}

// LoadBinds 全量读取粘性会话绑定（SCAN bind:* 前缀）。
func (u *Upstash) LoadBinds() map[string]string {
	out := map[string]string{}
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	iter := u.client.Scan(ctx, 0, bindPrefix+"*", 200).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		v, err := u.client.Get(ctx, key).Result()
		if err != nil {
			continue
		}
		out[strings.TrimPrefix(key, bindPrefix)] = v
	}
	return out
}

// Noop 纯内存降级：所有方法空实现。
type Noop struct{}

func (Noop) SetBind(string, string, time.Duration)      {}
func (Noop) DelBind(string)                             {}
func (Noop) LoadBinds() map[string]string               { return nil }
func (Noop) SaveState([]byte)                           {}
func (Noop) LoadState() ([]byte, bool)                  { return nil, false }
func (Noop) SaveResponse(string, []byte, time.Duration) {}
func (Noop) LoadResponse(string) ([]byte, bool)         { return nil, false }
