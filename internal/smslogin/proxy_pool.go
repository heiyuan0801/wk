package smslogin

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// PoolDialer 从一份静态代理名单里轮询出口 IP。
//
// Webshare 这类列表每条是一个固定 IP：一次登录全程用同一条（粘性），
// 下一次登录换下一条。用过的条目进入冷却，默认 30 分钟，对齐「同 IP
// 半小时只能注册一个号」的上游风控。
type PoolDialer struct {
	urls     []*url.URL
	cooldown time.Duration
	now      func() time.Time

	mu       sync.Mutex
	lastUsed []time.Time
	next     int
}

// LoadPoolDialer 从文本文件加载代理池。支持两种行格式：
//
//	host:port:user:pass
//	http://user:pass@host:port
//
// 空行和 # 注释忽略。cooldown <= 0 时按 30 分钟。
func LoadPoolDialer(path string, cooldown time.Duration) (*PoolDialer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取代理名单 %s: %w", path, err)
	}
	defer f.Close()

	var raw []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		raw = append(raw, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取代理名单 %s: %w", path, err)
	}
	return NewPoolDialer(raw, cooldown)
}

// NewPoolDialer 从内存行构造代理池。测试和配置层共用。
func NewPoolDialer(lines []string, cooldown time.Duration) (*PoolDialer, error) {
	if cooldown <= 0 {
		cooldown = 30 * time.Minute
	}
	var urls []*url.URL
	for i, line := range lines {
		u, err := ParseProxyLine(line)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行: %w", i+1, err)
		}
		if u == nil {
			continue
		}
		urls = append(urls, u)
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("代理名单为空")
	}
	return &PoolDialer{
		urls:     urls,
		cooldown: cooldown,
		now:      time.Now,
		lastUsed: make([]time.Time, len(urls)),
	}, nil
}

// Len 返回池子大小。
func (p *PoolDialer) Len() int { return len(p.urls) }

// ProxyEntry 是单条代理的可展示快照。
//
// 出于安全考虑**不含密码**：控制台只需看到出口主机和冷却状态，
// 凭据没有任何展示价值，却会在浏览器/日志里留下痕迹。
type ProxyEntry struct {
	Index int `json:"index"`
	// Host 出口主机（不含账密），形如 "1.2.3.4:8080"。
	Host string `json:"host"`
	// User 代理用户名（不含密码）。Webshare 多为固定值，便于辨认名单来源。
	User string `json:"user,omitempty"`
	// Used 是否被用过（false = 全新，还没消耗过冷却窗口）。
	Used bool `json:"used"`
	// Cooling 是否仍在冷却中（冷却中不可取用）。
	Cooling bool `json:"cooling"`
	// LastUsedAt 上次取用时间，零值省略。
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	// ReadyAt 何时冷却结束，冷却中才有值。
	ReadyAt *time.Time `json:"ready_at,omitempty"`
	// ReadyInSec 距可用还有多少秒，冷却中才有值。
	ReadyInSec int `json:"ready_in_sec,omitempty"`
}

// ProxyPoolSnapshot 是代理池的整体状态快照。
type ProxyPoolSnapshot struct {
	Total     int          `json:"total"`
	Available int          `json:"available"`
	Cooling   int          `json:"cooling"`
	Unused    int          `json:"unused"`
	Cooldown  string       `json:"cooldown"`
	Entries   []ProxyEntry `json:"entries"`
	// NextReadyInSec 全部冷却时，最早一条还有多久可用。
	NextReadyInSec int `json:"next_ready_in_sec,omitempty"`
}

// Snapshot 导出代理池状态，供控制台展示。
//
// 只在**调用瞬间**反映冷却情况：列表是静态的，冷却随取用滚动前进。
func (p *PoolDialer) Snapshot() ProxyPoolSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshotLocked()
}

// snapshotLocked 与 Snapshot 相同，但要求已持有 p.mu。
func (p *PoolDialer) snapshotLocked() ProxyPoolSnapshot {
	now := p.now()
	snap := ProxyPoolSnapshot{
		Total:    len(p.urls),
		Cooldown: p.cooldown.Round(time.Second).String(),
		Entries:  make([]ProxyEntry, 0, len(p.urls)),
	}
	nextReady := time.Duration(0)
	for i, u := range p.urls {
		entry := ProxyEntry{Index: i}
		if u != nil {
			entry.Host = u.Host
			if u.User != nil {
				// 只取用户名，密码永不外送。
				entry.User = u.User.Username()
			}
		}
		used := p.lastUsed[i]
		if !used.IsZero() {
			entry.Used = true
			at := used
			entry.LastUsedAt = &at
			if elapsed := now.Sub(used); elapsed < p.cooldown {
				wait := p.cooldown - elapsed
				entry.Cooling = true
				ready := now.Add(wait)
				entry.ReadyAt = &ready
				entry.ReadyInSec = int(wait.Round(time.Second) / time.Second)
				snap.Cooling++
				if nextReady == 0 || wait < nextReady {
					nextReady = wait
				}
			} else {
				snap.Available++
			}
		} else {
			snap.Unused++
			snap.Available++
		}
		snap.Entries = append(snap.Entries, entry)
	}
	if snap.Cooling == len(p.urls) && nextReady > 0 {
		snap.NextReadyInSec = int(nextReady.Round(time.Second) / time.Second)
	}
	return snap
}

// Dialer 取出下一条已冷却的代理。整池都在冷却时报错，避免静默复用触发风控。
func (p *PoolDialer) Dialer() (*url.URL, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.urls) == 0 {
		return nil, fmt.Errorf("代理池为空")
	}
	now := p.now()
	n := len(p.urls)
	var nextReady time.Duration
	for i := 0; i < n; i++ {
		idx := (p.next + i) % n
		used := p.lastUsed[idx]
		if used.IsZero() || now.Sub(used) >= p.cooldown {
			p.lastUsed[idx] = now
			p.next = (idx + 1) % n
			return cloneURL(p.urls[idx]), nil
		}
		wait := p.cooldown - now.Sub(used)
		if nextReady == 0 || wait < nextReady {
			nextReady = wait
		}
	}
	return nil, fmt.Errorf("全部 %d 条代理仍在冷却，约 %s 后可用", n, nextReady.Round(time.Second))
}

// ParseProxyLine 解析单行代理。空行 / 注释返回 (nil, nil)。
func ParseProxyLine(line string) (*url.URL, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil, nil
	}
	if strings.Contains(line, "://") {
		u, err := url.Parse(line)
		if err != nil {
			return nil, fmt.Errorf("代理地址不合法: %w", err)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("代理地址缺少 host: %q", line)
		}
		if u.Scheme == "" {
			u.Scheme = "http"
		}
		return u, nil
	}
	// host:port:user:pass —— 从右侧拆，用户名里一般不含冒号，host 可能是 IPv6 但
	// Webshare 给的是 IPv4。固定 4 段。
	parts := strings.Split(line, ":")
	if len(parts) != 4 {
		return nil, fmt.Errorf("无法解析 %q，期望 host:port:user:pass", line)
	}
	host, port, user, pass := parts[0], parts[1], parts[2], parts[3]
	if host == "" || port == "" {
		return nil, fmt.Errorf("无法解析 %q，缺少 host/port", line)
	}
	u := &url.URL{
		Scheme: "http",
		Host:   host + ":" + port,
		User:   url.UserPassword(user, pass),
	}
	return u, nil
}

func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	cp := *u
	if u.User != nil {
		if pass, ok := u.User.Password(); ok {
			cp.User = url.UserPassword(u.User.Username(), pass)
		} else {
			cp.User = url.User(u.User.Username())
		}
	}
	return &cp
}

var _ ProxyDialer = (*PoolDialer)(nil)
