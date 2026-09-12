package smslogin

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ProxyDialer 为一次短信登录会话提供出口代理。
//
// 背景：同一 IP 半小时内只能注册一个号，多次注册会被上游风控。因此把整条
// 登录链路（OneID/Keycloak/Console/CLI 四条）放到一个住宅代理后面，并且
// 每次登录换一个新出口 IP。
//
// 抽象成接口而非直接写死某个代理商：出网方式属于部署差异，测试也要能
// 用假实现替代。
type ProxyDialer interface {
	// Dialer 返回这次登录要用的代理。返回 nil 表示这次不走代理。
	Dialer() (*url.URL, error)
}

// ResolverProxyDialer 用一个固定的上游代理地址构造每次随机的会话参数。
//
// 以 1024proxy 为例，粘性会话通过用户名后缀携带：
//
//	user-sid-<随机值>-t-30
//
// sid 决定出口 IP，t 是粘性时长（分钟）。每次登录生成新 sid，就得到一个
// 全新的住宅 IP，而这一次登录内部的十几个请求又始终落在同一个 IP 上。
type ResolverProxyDialer struct {
	// Address 形如 http://user:pass@host:port。sid 注入到 user 里。
	Address string
	// Region 可选的 ISO 3166-1 两位码（如 HK）。拼进用户名：user-region-HK-sid-...。
	// 空 = 不指定地区。用户名里已经带 -region- 时不再重复注入。
	Region string
	// InjectSID 为 true 时按 1024proxy 约定改写用户名（-sid-<随机>-t-<分钟>）。
	// 普通静态代理必须关掉，否则账密会被改坏。
	InjectSID bool
	// StickyMinutes 粘性时长，1-120。登录流程通常几分钟内完成，30 足够。
	StickyMinutes int
	// Timeout 单次拨号超时。
	Timeout time.Duration

	randSID func() string
}

// NewResolverProxyDialer 构造一个每次登录换 IP 的拨号器。
func NewResolverProxyDialer(address string, stickyMinutes int) *ResolverProxyDialer {
	return NewResolverProxyDialerWithRegion(address, "", stickyMinutes)
}

// NewResolverProxyDialerWithRegion 同上，额外钉死出口地区。
func NewResolverProxyDialerWithRegion(address, region string, stickyMinutes int) *ResolverProxyDialer {
	d := &ResolverProxyDialer{
		Address:       strings.TrimSpace(address),
		Region:        strings.ToUpper(strings.TrimSpace(region)),
		InjectSID:     false,
		StickyMinutes: stickyMinutes,
		Timeout:       20 * time.Second,
		randSID:       randSessionID,
	}
	if d.StickyMinutes <= 0 {
		// 半小时风控窗口 > 登录时长，30 分钟足够且不超过代理上限。
		d.StickyMinutes = 30
	}
	if d.StickyMinutes > 120 {
		d.StickyMinutes = 120
	}
	return d
}

// Dialer 实现接口：把随机 sid 拼进用户名。
func (d *ResolverProxyDialer) Dialer() (*url.URL, error) {
	raw := strings.TrimSpace(d.Address)
	if raw == "" {
		return nil, fmt.Errorf("代理地址为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("代理地址不合法: %w", err)
	}
	if u.User != nil && d.InjectSID {
		username := u.User.Username()
		// sid 段必须放在用户名里（1024proxy 的会话控制约定），密码不动。
		// 地区可选：user[-region-XX]-sid-<随机>-t-<分钟>
		if d.Region != "" && !strings.Contains(strings.ToLower(username), "-region-") {
			username = fmt.Sprintf("%s-region-%s", username, d.Region)
		}
		sidUser := fmt.Sprintf("%s-sid-%s-t-%d", username, d.randSID(), d.StickyMinutes)
		if pass, ok := u.User.Password(); ok {
			u.User = url.UserPassword(sidUser, pass)
		} else {
			u.User = url.User(sidUser)
		}
	}
	if u.Host == "" {
		return nil, fmt.Errorf("代理地址缺少 host: %q", raw)
	}
	return u, nil
}

func randSessionID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// 退化为时间戳，保证 sid 仍基本唯一
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// directDialer 永远不走代理。未配置代理时用它，保持行为与现在完全一致。
type directDialer struct{}

func (directDialer) Dialer() (*url.URL, error) { return nil, nil }

// proxyTransport 给单个登录会话构造带代理的 Transport。
//
// 每个会话独立一份 Transport：代理是会话级的（sid 绑定这次登录），
// 复用全局 Transport 会让不同登录串到同一个出口 IP。
func proxyTransport(proxyURL *url.URL, timeout time.Duration) *http.Transport {
	t := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        8,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: timeout,
	}
	// 代理握手阶段就发现认证失败，而不是等到第一个请求
	t.ResponseHeaderTimeout = timeout
	return t
}

// applyProxy 给登录会话的四个 client 统一装上同一个代理出口。
//
// 返回的 cleanup 在会话结束时应被调用（关闭空闲连接）。
func (m *Manager) applyProxy(s *session) (func(), error) {
	if m.proxy == nil {
		return func() {}, nil
	}
	proxyURL, err := m.proxy.Dialer()
	if err != nil {
		return func() {}, fmt.Errorf("获取登录代理失败: %w", err)
	}
	if proxyURL == nil {
		return func() {}, nil
	}

	base := proxyTransport(proxyURL, m.proxyTimeout())
	transport := wrapCookieFix(base)
	setTransport := func(c *http.Client) {
		c.Transport = transport
	}
	setTransport(s.oneIDHTTP)
	setTransport(s.consoleHTTP)
	setTransport(s.consoleNoJump)
	setTransport(s.cliHTTP)
	return func() {
		base.CloseIdleConnections()
	}, nil
}

func (m *Manager) proxyTimeout() time.Duration {
	if m.proxyDialTimeout > 0 {
		return m.proxyDialTimeout
	}
	return 20 * time.Second
}

// SetProxyDialer 配置登录代理。nil = 直连（默认）。
func (m *Manager) SetProxyDialer(d ProxyDialer) { m.proxy = d }

// SetProxyTimeout 拨号超时。
func (m *Manager) SetProxyTimeout(d time.Duration) { m.proxyDialTimeout = d }

var _ ProxyDialer = (*ResolverProxyDialer)(nil)
var _ ProxyDialer = directDialer{}
