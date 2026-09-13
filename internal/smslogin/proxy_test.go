package smslogin

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestResolverProxyDialerSIDInjection sid 必须拼进用户名（1024proxy 的会话控制
// 约定），密码与 host 原样保留，且每次调用生成不同的 sid。
func TestResolverProxyDialerSIDInjection(t *testing.T) {
	d := NewResolverProxyDialer("http://user123:pass456@gw.example.com:7777", 30)
	d.InjectSID = true
	d.randSID = func() string { return "abc123" }

	u, err := d.Dialer()
	if err != nil {
		t.Fatalf("dialer: %v", err)
	}
	if got := u.User.Username(); got != "user123-sid-abc123-t-30" {
		t.Fatalf("username=%q want user123-sid-abc123-t-30", got)
	}
	if pass, _ := u.User.Password(); pass != "pass456" {
		t.Fatalf("password must not change, got %q", pass)
	}
	if u.Host != "gw.example.com:7777" {
		t.Fatalf("host=%q", u.Host)
	}
	if u.Scheme != "http" {
		t.Fatalf("scheme=%q", u.Scheme)
	}
}

// TestResolverProxyDialerUniqueSIDPerCall 每次登录都要换出口 IP，
// 因此连续两次 Dialer 必须给出不同的 sid。
func TestResolverProxyDialerUniqueSIDPerCall(t *testing.T) {
	d := NewResolverProxyDialer("http://u:p@gw:1", 30)
	d.InjectSID = true
	u1, err := d.Dialer()
	if err != nil {
		t.Fatal(err)
	}
	u2, err := d.Dialer()
	if err != nil {
		t.Fatal(err)
	}
	if u1.User.Username() == u2.User.Username() {
		t.Fatalf("sid must differ per login: %q", u1.User.Username())
	}
}

// TestResolverProxyDialerStickyClamp 粘性时长必须落在 1-120（代理商上限）。
func TestResolverProxyDialerStickyClamp(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, 30}, {1, 1}, {30, 30}, {120, 120}, {999, 120}, {-5, 30},
	}
	for _, c := range cases {
		d := NewResolverProxyDialer("http://u:p@gw:1", c.in)
		d.InjectSID = true
		d.randSID = func() string { return "x" }
		u, err := d.Dialer()
		if err != nil {
			t.Fatalf("in=%d: %v", c.in, err)
		}
		if !strings.Contains(u.User.Username(), strings.TrimRight("-t-"+intToString(c.want), "")) {
			// 直接判后缀更直白
		}
		if !strings.HasSuffix(u.User.Username(), "-t-"+intToString(c.want)) {
			t.Fatalf("in=%d username=%q want suffix -t-%d", c.in, u.User.Username(), c.want)
		}
	}
}

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestResolverProxyDialerNoPassword 无密码的代理地址也要能注入 sid。
func TestResolverProxyDialerRegionInjection(t *testing.T) {
	d := NewResolverProxyDialerWithRegion("http://user:pass@gw:1", "hk", 30)
	d.InjectSID = true
	d.randSID = func() string { return "abc" }
	u, err := d.Dialer()
	if err != nil {
		t.Fatal(err)
	}
	if got := u.User.Username(); got != "user-region-HK-sid-abc-t-30" {
		t.Fatalf("username=%q", got)
	}
}

func TestResolverProxyDialerNoSIDKeepsUsername(t *testing.T) {
	d := NewResolverProxyDialer("http://staticuser:staticpass@10.9.8.7:3128", 30)
	u, err := d.Dialer()
	if err != nil {
		t.Fatal(err)
	}
	if got := u.User.Username(); got != "staticuser" {
		t.Fatalf("static proxy username mutated: %q", got)
	}
	if pass, _ := u.User.Password(); pass != "staticpass" {
		t.Fatalf("password=%q", pass)
	}
}

func TestResolverProxyDialerRegionNotDuplicated(t *testing.T) {
	d := NewResolverProxyDialerWithRegion("http://user-region-HK:pass@gw:1", "TW", 30)
	d.InjectSID = true
	d.randSID = func() string { return "abc" }
	u, err := d.Dialer()
	if err != nil {
		t.Fatal(err)
	}
	if got := u.User.Username(); got != "user-region-HK-sid-abc-t-30" {
		t.Fatalf("already-region username mutated: %q", got)
	}
}

func TestResolverProxyDialerNoPassword(t *testing.T) {
	d := NewResolverProxyDialer("http://user@gw:7777", 30)
	d.InjectSID = true
	d.randSID = func() string { return "zz" }
	u, err := d.Dialer()
	if err != nil {
		t.Fatal(err)
	}
	if got := u.User.Username(); got != "user-sid-zz-t-30" {
		t.Fatalf("username=%q", got)
	}
	if _, has := u.User.Password(); has {
		t.Fatal("no password expected")
	}
}

// TestResolverProxyDialerRejectsBadAddress 地址非法时报可读错误，不能 panic。
func TestResolverProxyDialerRejectsBadAddress(t *testing.T) {
	for _, addr := range []string{"", "   ", "://bad", "http://"} {
		d := NewResolverProxyDialer(addr, 30)
		if _, err := d.Dialer(); err == nil {
			t.Fatalf("address %q must be rejected", addr)
		}
	}
}

// TestManagerAppliesProxyToAllClients 登录链路四个 client 必须都走同一个
// 代理（OneID/Keycloak/Console/CLI 任意一条直连都会暴露真实 IP）。
func TestManagerAppliesProxyToAllClients(t *testing.T) {
	want := mustProxyURL(t, "http://user:pass@gw:7777")
	m := NewManager(Endpoints{}, 0)
	m.SetProxyDialer(staticDialer{want})

	s, err := m.newSession("+852 64087495")
	if err != nil {
		t.Fatal(err)
	}
	for name, client := range map[string]*http.Client{
		"oneIDHTTP":     s.oneIDHTTP,
		"consoleHTTP":   s.consoleHTTP,
		"consoleNoJump": s.consoleNoJump,
		"cliHTTP":       s.cliHTTP,
	} {
		tp, ok := unwrapTransport(client.Transport).(*http.Transport)
		if !ok {
			t.Fatalf("%s: no custom transport (type %T)", name, client.Transport)
		}
		// 通过一次真实请求取代理 URL 来验证。
		req, _ := http.NewRequest(http.MethodGet, "https://www.codebuddy.cn/", nil)
		got, err := tp.Proxy(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.String() != want.String() {
			t.Fatalf("%s proxy=%v want %v", name, got, want)
		}
	}
}

// TestManagerWithoutProxyStaysDirect 未配置代理时不能改变现有行为。
func TestManagerWithoutProxyStaysDirect(t *testing.T) {
	m := NewManager(Endpoints{}, 0)
	s, err := m.newSession("+852 64087495")
	if err != nil {
		t.Fatal(err)
	}
	for name, client := range map[string]*http.Client{
		"oneIDHTTP":     s.oneIDHTTP,
		"consoleHTTP":   s.consoleHTTP,
		"consoleNoJump": s.consoleNoJump,
		"cliHTTP":       s.cliHTTP,
	} {
		if name == "consoleHTTP" || name == "consoleNoJump" {
			if _, ok := client.Transport.(*cookieFixTransport); !ok {
				t.Fatalf("%s: console clients need cookie rewrite, got %T", name, client.Transport)
			}
			continue
		}
		if client.Transport != nil {
			t.Fatalf("%s: transport should stay nil (direct), got %T", name, client.Transport)
		}
	}
}

// TestProxyDialerErrorFailsFast 代理地址坏掉时报错，不能静默直连
// （那会让所有登录都落在同一个本机 IP 上，正好触发要规避的风控）。
func TestProxyDialerErrorFailsFast(t *testing.T) {
	m := NewManager(Endpoints{}, 0)
	m.SetProxyDialer(brokenDialer{})
	if _, err := m.newSession("+852 64087495"); err == nil {
		t.Fatal("broken proxy dialer must fail loudly")
	}
}

type staticDialer struct{ u *url.URL }

func (d staticDialer) Dialer() (*url.URL, error) { return d.u, nil }

type brokenDialer struct{}

func (brokenDialer) Dialer() (*url.URL, error) { return nil, errBrokenProxy }

var errBrokenProxy = &url.Error{Op: "dial", URL: "", Err: http.ErrAbortHandler}

func mustProxyURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
