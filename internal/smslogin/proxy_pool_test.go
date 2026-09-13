package smslogin

import (
	"encoding/json"
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

// TestPoolSnapshotNeverLeaksPassword 快照是给控制台看的，绝不能带密码。
func TestPoolSnapshotNeverLeaksPassword(t *testing.T) {
	now := time.Date(2026, 9, 12, 17, 0, 0, 0, time.UTC)
	p, err := NewPoolDialer([]string{
		"1.1.1.1:1:alice:SUPERSECRETPW",
		"http://bob:ANOTHERPW@2.2.2.2:2",
	}, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return now }

	// 序列化后再查，确保任何字段都不会夹带密码。
	raw, err := json.Marshal(p.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SUPERSECRETPW", "ANOTHERPW"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("snapshot leaked %q: %s", secret, raw)
		}
	}
	// 主机和用户名要保留（否则页面没东西可显示）。
	if !strings.Contains(string(raw), "1.1.1.1:1") || !strings.Contains(string(raw), "alice") {
		t.Fatalf("snapshot should keep host/user: %s", raw)
	}
}

// TestPoolSnapshotCooldownStates 快照要如实反映"全新/可用/冷却中"三种状态。
func TestPoolSnapshotCooldownStates(t *testing.T) {
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

	// 取一条用掉，另外两条保持全新。
	if _, err := p.Dialer(); err != nil {
		t.Fatal(err)
	}
	snap := p.Snapshot()
	if snap.Total != 3 {
		t.Fatalf("total=%d want 3", snap.Total)
	}
	if snap.Cooling != 1 {
		t.Fatalf("cooling=%d want 1", snap.Cooling)
	}
	if snap.Unused != 2 {
		t.Fatalf("unused=%d want 2", snap.Unused)
	}
	if snap.Available != 2 {
		t.Fatalf("available=%d want 2", snap.Available)
	}
	// 被用掉的那条：used + cooling + 剩余时间 ≈ 30 分钟。
	used := snap.Entries[0]
	if !used.Used || !used.Cooling {
		t.Fatalf("entry 0 should be used+cooling: %+v", used)
	}
	if used.ReadyInSec <= 0 || used.ReadyInSec > 1800 {
		t.Fatalf("ready_in_sec=%d want (0,1800]", used.ReadyInSec)
	}
	if used.ReadyAt == nil || !used.ReadyAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("ready_at=%v want %v", used.ReadyAt, now.Add(30*time.Minute))
	}
	// 全新条目不该有冷却字段。
	if snap.Entries[1].Used || snap.Entries[1].Cooling || snap.Entries[1].ReadyAt != nil {
		t.Fatalf("entry 1 should be fresh: %+v", snap.Entries[1])
	}

	// 冷却结束后转为可用。
	now = now.Add(31 * time.Minute)
	snap = p.Snapshot()
	if snap.Cooling != 0 || snap.Available != 3 {
		t.Fatalf("after cooldown: cooling=%d available=%d want 0/3", snap.Cooling, snap.Available)
	}
}

// TestPoolSnapshotAllCoolingReportsNextReady 整池冷却时要给出最早可用时间，
// 否则页面只能说"不可用"而说不出"还要等多久"。
func TestPoolSnapshotAllCoolingReportsNextReady(t *testing.T) {
	now := time.Date(2026, 9, 12, 17, 0, 0, 0, time.UTC)
	p, err := NewPoolDialer([]string{"1.1.1.1:1:u:p", "2.2.2.2:2:u:p"}, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if _, err := p.Dialer(); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute) // 第二条稍晚使用
	}
	snap := p.Snapshot()
	if snap.Cooling != 2 || snap.Available != 0 {
		t.Fatalf("cooling=%d available=%d want 2/0", snap.Cooling, snap.Available)
	}
	// 最早那条是第一次取的，距 30 分钟还差 29 分钟。
	if snap.NextReadyInSec <= 0 {
		t.Fatalf("next_ready_in_sec=%d want >0", snap.NextReadyInSec)
	}
	if snap.NextReadyInSec > 1740 {
		t.Fatalf("next_ready_in_sec=%d want <=1740 (first used 1min ago)", snap.NextReadyInSec)
	}
}

// TestManagerProxyStatusNilWhenDirect 未配置代理时不该谎报代理信息。
func TestManagerProxyStatusNilWhenDirect(t *testing.T) {
	m := NewManager(Endpoints{}, time.Minute)
	if got := m.ProxyStatus(); got != nil {
		t.Fatalf("direct manager should report nil, got %v", got)
	}
}

// TestManagerProxyStatusPoolAndResolver 两种拨号器各自导出正确形态。
func TestManagerProxyStatusPoolAndResolver(t *testing.T) {
	m := NewManager(Endpoints{}, time.Minute)
	p, err := NewPoolDialer([]string{"1.1.1.1:1:alice:PW1", "2.2.2.2:2:bob:PW2"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	m.SetProxyDialer(p)
	st := m.ProxyStatus()
	if st["mode"] != "pool" {
		t.Fatalf("mode=%v want pool", st["mode"])
	}
	if st["endpoints"] != 2 {
		t.Fatalf("endpoints=%v want 2", st["endpoints"])
	}
	raw, _ := json.Marshal(st)
	for _, secret := range []string{"PW1", "PW2"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("status leaked %q: %s", secret, raw)
		}
	}

	// resolver 形态
	m2 := NewManager(Endpoints{}, time.Minute)
	m2.SetProxyDialer(NewResolverProxyDialerWithRegion("http://u:RESOLVERPW@gw.example.com:7777", "hk", 30))
	st2 := m2.ProxyStatus()
	if st2["mode"] != "resolver" {
		t.Fatalf("mode=%v want resolver", st2["mode"])
	}
	if st2["host"] != "gw.example.com:7777" {
		t.Fatalf("host=%v", st2["host"])
	}
	if st2["region"] != "HK" {
		t.Fatalf("region=%v want HK", st2["region"])
	}
	raw2, _ := json.Marshal(st2)
	if strings.Contains(string(raw2), "RESOLVERPW") {
		t.Fatalf("resolver status leaked password: %s", raw2)
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
