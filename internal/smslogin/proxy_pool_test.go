package smslogin

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseProxyLineWebshare(t *testing.T) {
	u, err := ParseProxyLine("10.1.2.3:8080:proxyuser:proxypass")
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "http" || u.Host != "10.1.2.3:8080" {
		t.Fatalf("host=%s scheme=%s", u.Host, u.Scheme)
	}
	if u.User.Username() != "proxyuser" {
		t.Fatalf("user=%q", u.User.Username())
	}
	if pass, _ := u.User.Password(); pass != "proxypass" {
		t.Fatalf("pass=%q", pass)
	}
}

func TestParseProxyLineURLAndSkip(t *testing.T) {
	u, err := ParseProxyLine("http://user:secret@10.4.5.6:3128")
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "10.4.5.6:3128" {
		t.Fatalf("host=%s", u.Host)
	}
	if _, err := ParseProxyLine(""); err != nil {
		t.Fatal(err)
	}
	if u, err := ParseProxyLine("# comment"); err != nil || u != nil {
		t.Fatalf("comment: %v %v", u, err)
	}
	if _, err := ParseProxyLine("only:three:parts"); err == nil {
		t.Fatal("malformed line must fail")
	}
}

func TestPoolRoundRobinAndCooldown(t *testing.T) {
	now := time.Date(2026, 9, 12, 17, 0, 0, 0, time.UTC)
	p, err := NewPoolDialer([]string{
		"1.1.1.1:1:u:p",
		"2.2.2.2:2:u:p",
		"3.3.3.3:3:u:p",
	}, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return now }

	got := map[string]int{}
	for i := 0; i < 3; i++ {
		u, err := p.Dialer()
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		got[u.Host]++
	}
	if len(got) != 3 {
		t.Fatalf("want 3 distinct hosts, got %v", got)
	}
	if _, err := p.Dialer(); err == nil || !strings.Contains(err.Error(), "冷却") {
		t.Fatalf("exhausted pool must fail, got %v", err)
	}

	now = now.Add(30 * time.Minute)
	u, err := p.Dialer()
	if err != nil {
		t.Fatalf("after cooldown: %v", err)
	}
	if u.Host != "1.1.1.1:1" {
		t.Fatalf("round-robin should resume at first host, got %s", u.Host)
	}
}

func TestPoolLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxies.txt")
	body := "# comment\n\n10.0.0.1:8080:alice:secret\nhttp://bob:pw@10.0.0.2:9\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPoolDialer(path, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if p.Len() != 2 {
		t.Fatalf("len=%d", p.Len())
	}
	u, err := p.Dialer()
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "10.0.0.1:8080" {
		t.Fatalf("first=%s", u.Host)
	}
}

func TestPoolAppliesToLoginClients(t *testing.T) {
	p, err := NewPoolDialer([]string{"9.9.9.9:9:u:p"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(Endpoints{}, 0)
	m.SetProxyDialer(p)
	s, err := m.newSession("+852 64087495")
	if err != nil {
		t.Fatal(err)
	}
	want := mustProxyURL(t, "http://u:p@9.9.9.9:9")
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
