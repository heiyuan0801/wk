package smslogin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUpstream 用 httptest 复刻三个上游主机的行为，让整条短信登录链路可以在
// 无网络、无真实短信的情况下端到端跑完。
type fakeUpstream struct {
	console *httptest.Server
	cli     *httptest.Server
	oneID   *httptest.Server

	mu sync.Mutex
	// 调用痕迹，供断言"确实按顺序走了每一步"。
	calls []string
	// 写票是否成功。false 模拟漏带 Authorization 的情况。
	ticketWritten bool
	// 一次性票，写票后可用。
	pendingTicket bool
	// 是否已在 console jar 中看到 APISIX session。
	sawAPISIXSession bool
	// 强制给 Keycloak 登录页返回 302（已有 SSO 会话），用于验证防串号。
	forceSSO bool
	// OneID 账号数量，>1 用于验证多账号拒绝分支。
	accountCount int
}

func newFakeUpstream() *fakeUpstream {
	f := &fakeUpstream{ticketWritten: false, accountCount: 1}

	f.cli = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record("cli " + r.URL.Path)
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			writeEnvelope(w, map[string]any{
				"state":   "cli-state-1",
				"authUrl": f.consoleHost() + "/login/?platform=CLI&state=cli-state-1",
			})
		case "/v2/plugin/auth/token":
			if !f.pendingTicket {
				writeJSONRaw(w, 200, `{"code":11217,"msg":"11217:login ing..."}`)
				return
			}
			f.mu.Lock()
			f.pendingTicket = false
			f.mu.Unlock()
			writeEnvelope(w, map[string]any{
				"accessToken":  "at-1",
				"refreshToken": "rt-1",
				"expiresIn":    5184000,
				"domain":       "www.codebuddy.cn",
			})
		case "/v2/plugin/login/account":
			if r.Header.Get("Authorization") != "Bearer at-1" {
				writeJSONRaw(w, 401, `{"code":401,"msg":"unauthorized"}`)
				return
			}
			writeEnvelope(w, map[string]any{
				"uid":          "uid-1",
				"enterpriseId": "",
				"nickname":     "64087495",
			})
		default:
			writeJSONRaw(w, 404, `{"code":404,"msg":"not found"}`)
		}
	}))

	f.oneID = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record("oneid " + r.URL.Path)
		switch r.URL.Path {
		case "/v1/auth/sms/code/send":
			var req struct {
				ClientCode string   `json:"client_code"`
				Mobile     string   `json:"mobile"`
				Scopes     []string `json:"scopes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.ClientCode != "codebuddy" {
				writeJSONRaw(w, 400, `{"errCode":"E0010343","errMessage":"参数【client_code】错误"}`)
				return
			}
			// 文档明确：香港号必须是 "+852 64087495"，区号与号码之间有空格。
			if !strings.Contains(req.Mobile, " ") {
				writeJSONRaw(w, 400, `{"errCode":"E0010001","errMessage":"请求参数不合法"}`)
				return
			}
			writeJSONRaw(w, 200, `{"status":"unexpired","state_token":"tok-send","expires_in":300}`)
		case "/v1/auth/sms/code/verify":
			var req struct {
				StateToken string `json:"state_token"`
				Code       string `json:"code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Code != "123456" {
				// OneID 会把 token 回显在错误里，正是 redact 要处理的形态。
				writeJSONRaw(w, 400, fmt.Sprintf(`{"errCode":"E0010072","errMessage":"无效的token%s"}`, req.StateToken))
				return
			}
			writeJSONRaw(w, 200, `{"state_token":"tok-verify"}`)
		case "/v1/auth/accounts":
			if r.URL.Query().Get("state_token") != "tok-verify" {
				writeJSONRaw(w, 400, `{"errCode":"E0010072","errMessage":"无效的token"}`)
				return
			}
			accounts := make([]map[string]any, 0, f.accountCount)
			for i := 0; i < f.accountCount; i++ {
				accounts = append(accounts, map[string]any{
					"id": "codebuddy@", "name": fmt.Sprintf("6408749%d", i), "type": 2,
				})
			}
			writeJSONRaw(w, 200, mustJSON(map[string]any{
				"accounts": accounts, "code": "oneid-code-1", "next_step": "login",
			}))
		default:
			writeJSONRaw(w, 404, `{"errCode":"E404","errMessage":"not found"}`)
		}
	}))

	f.console = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record("console " + r.URL.Path)
		switch {
		case r.URL.Path == "/auth/realms/copilot/protocol/openid-connect/auth":
			if f.forceSSO {
				w.Header().Set("Location", "/login/?code=stale")
				w.WriteHeader(302)
				return
			}
			setCookie(w, "AUTH_SESSION_ID", "auth-session-1", "/auth/realms/copilot")
			w.Header().Set("Content-Type", "text/html")
			// href 里的 & 以 &amp; 形式出现，正是真实页面的形态。
			_, _ = fmt.Fprintf(w, `<a data-idp="oneid" href="%s/auth/realms/copilot/broker/oneid/login?client_id=console&amp;tab_id=t1&amp;session_code=sc1">OneID</a>`,
				f.consoleURL())
		case r.URL.Path == "/auth/realms/copilot/broker/oneid/login":
			if r.URL.Query().Get("from_oneid_login") != "true" {
				writeJSONRaw(w, 400, `{"code":400,"msg":"missing from_oneid_login"}`)
				return
			}
			writeEnvelope(w, map[string]any{
				"state":        "broker-state-1",
				"redirect_uri": f.consoleURL() + "/auth/realms/copilot/broker/oneid/endpoint",
			})
		case r.URL.Path == "/auth/realms/copilot/broker/oneid/endpoint":
			if r.URL.Query().Get("code") != "oneid-code-1" || r.URL.Query().Get("state") != "broker-state-1" {
				writeJSONRaw(w, 400, `{"code":400,"msg":"bad broker callback"}`)
				return
			}
			// 真实环境这里连跳多级，最终落 KEYCLOAK_IDENTITY。
			setCookie(w, "KEYCLOAK_IDENTITY", "kc-identity-1", "/auth/realms/copilot")
			w.Header().Set("Location", "/login/?platform=CLI&state=cli-state-1&code=kc-code")
			w.WriteHeader(302)
		case r.URL.Path == "/login/":
			// broker 链的落点：真实站点在这里返回登录页 HTML。
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html>login</html>`))
		case r.URL.Path == "/console/accounts":
			// APISIX 网关：无 session 时去 Keycloak 换，然后设 session。
			// 按请求 Cookie 判定，保证每个新会话（新 jar）都要各自走一次 OIDC。
			if !f.hasAPISIXSession(r) {
				setCookie(w, "session", "apisix-session-1", "/")
				w.Header().Set("Location", "/console/accounts")
				w.WriteHeader(302)
				return
			}
			writeJSONRaw(w, 200, `[{"nickname":"64087495","pluginEnabled":true}]`)
		case r.URL.Path == "/console/login/enterprise":
			if !f.hasAPISIXSession(r) {
				writeJSONRaw(w, 401, `{"code":401,"msg":"no apisix session"}`)
				return
			}
			writeEnvelope(w, map[string]any{"accessToken": "console-at-1", "tokenType": "Bearer"})
		case r.URL.Path == "/console/auth/login":
			// 必须显式带 Bearer，否则写不上票。
			if r.Header.Get("Authorization") != "Bearer console-at-1" {
				w.Header().Set("Location", "/login?force_login_type=&platform=CLI&state=cli-state-1")
				w.WriteHeader(302)
				return
			}
			f.mu.Lock()
			f.pendingTicket = true
			f.ticketWritten = true
			f.mu.Unlock()
			// 302 到站点根 = 写票成功。
			w.Header().Set("Location", "/")
			w.WriteHeader(302)
		default:
			writeJSONRaw(w, 404, `{"code":404,"msg":"not found"}`)
		}
	}))
	return f
}

func (f *fakeUpstream) close() {
	f.console.Close()
	f.cli.Close()
	f.oneID.Close()
}

func (f *fakeUpstream) endpoints() Endpoints {
	return Endpoints{Console: f.consoleURL(), CLI: f.cli.URL, OneID: f.oneID.URL, Realm: "copilot"}
}

func (f *fakeUpstream) consoleURL() string  { return f.console.URL }
func (f *fakeUpstream) consoleHost() string { return f.console.URL }
func (f *fakeUpstream) record(s string)     { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }
func (f *fakeUpstream) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}
func (f *fakeUpstream) wasWritten() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.ticketWritten }
func (f *fakeUpstream) hasAPISIXSession(r *http.Request) bool {
	c, err := r.Cookie("session")
	return err == nil && c.Value == "apisix-session-1"
}

func writeEnvelope(w http.ResponseWriter, data any) {
	writeJSONRaw(w, 200, mustJSON(map[string]any{"code": 0, "msg": "OK", "data": data}))
}

func writeJSONRaw(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func setCookie(w http.ResponseWriter, name, value, path string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: path})
}

// ---------------------------------------------------------------------------
// 测试
// ---------------------------------------------------------------------------

func TestSendAndVerifyFullFlow(t *testing.T) {
	f := newFakeUpstream()
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if send.SessionID == "" || send.Mobile != "+852 64087495" {
		t.Fatalf("send result=%+v", send)
	}

	creds, err := m.Verify(context.Background(), send.SessionID, "123456")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if creds.UID != "uid-1" || creds.AccessToken != "at-1" || creds.RefreshToken != "rt-1" {
		t.Fatalf("credentials=%+v", creds)
	}
	if creds.Nickname != "64087495" || creds.Region != "cn" {
		t.Fatalf("credentials=%+v", creds)
	}
	if creds.ExpiresIn != 5184000 || creds.Domain != "www.codebuddy.cn" {
		t.Fatalf("credentials=%+v", creds)
	}
	if !f.wasWritten() {
		t.Fatal("expected the one-time ticket to be written")
	}

	// 按文档顺序走完全部步骤。重定向落点（/login/ 与第二次 /console/accounts）
	// 也在其中：它们证明 broker 链与 APISIX OIDC 都真的被跟随了。
	want := []string{
		"cli /v2/plugin/auth/state",
		"oneid /v1/auth/sms/code/send",
		"oneid /v1/auth/sms/code/verify",
		"oneid /v1/auth/accounts",
		"console /auth/realms/copilot/protocol/openid-connect/auth",
		"console /auth/realms/copilot/broker/oneid/login",
		"console /auth/realms/copilot/broker/oneid/endpoint",
		"console /login/",
		"console /console/accounts",
		"console /console/accounts",
		"console /console/login/enterprise",
		"console /console/auth/login",
		"cli /v2/plugin/auth/token",
		"cli /v2/plugin/login/account",
	}
	got := f.callList()
	if len(got) != len(want) {
		t.Fatalf("call count=%d want %d\ngot=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call[%d]=%q want %q\nfull=%v", i, got[i], want[i], got)
		}
	}
}

// TestVerifyConsumesSession 一次性语义：无论成败都不可重放，避免复用已被上游
// 作废的 OneID code / Keycloak 会话。
func TestVerifyConsumesSession(t *testing.T) {
	f := newFakeUpstream()
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := m.Verify(context.Background(), send.SessionID, "000000"); err == nil {
		t.Fatal("wrong code must fail")
	}
	if _, err := m.Verify(context.Background(), send.SessionID, "123456"); err == nil {
		t.Fatal("session must not be reusable after a failed attempt")
	}
}

func TestVerifyUnknownSession(t *testing.T) {
	m := NewManager(DefaultEndpoints(), 0)
	if _, err := m.Verify(context.Background(), "nope", "123456"); err != ErrSessionNotFound {
		t.Fatalf("err=%v want ErrSessionNotFound", err)
	}
	if _, err := m.Verify(context.Background(), "", "123456"); err != ErrSessionNotFound {
		t.Fatalf("empty session err=%v want ErrSessionNotFound", err)
	}
}

// TestErrorRedactsToken 上游会把 state_token 回显进错误消息，向上传播前必须抹掉。
func TestErrorRedactsToken(t *testing.T) {
	f := newFakeUpstream()
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	_, err = m.Verify(context.Background(), send.SessionID, "999999")
	if err == nil {
		t.Fatal("expected failure")
	}
	if strings.Contains(err.Error(), "tok-send") {
		t.Fatalf("error leaks state_token: %v", err)
	}
	if !strings.Contains(err.Error(), "E0010072") {
		t.Fatalf("error should keep the oneid error code: %v", err)
	}
}

// TestRefusesReusedKeycloakSession 复用 Cookie 会静默登成上一个账号，必须失败。
func TestRefusesReusedKeycloakSession(t *testing.T) {
	f := newFakeUpstream()
	f.forceSSO = true
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	_, err = m.Verify(context.Background(), send.SessionID, "123456")
	if err == nil {
		t.Fatal("must abort when a stale Keycloak SSO session is detected")
	}
	if !strings.Contains(err.Error(), "复用的 Keycloak 会话") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRefusesMultipleOneIDAccounts 多账号需要额外的选择步骤，宁可不做也不猜。
func TestRefusesMultipleOneIDAccounts(t *testing.T) {
	f := newFakeUpstream()
	f.accountCount = 2
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	_, err = m.Verify(context.Background(), send.SessionID, "123456")
	if err == nil {
		t.Fatal("multiple accounts must be refused")
	}
	if !strings.Contains(err.Error(), "多个账号") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestWriteTicketFailureIsReported 漏带 Bearer 时上游 302 回 /login，
// 这种"看起来像成功"的跳转必须判定为失败。
func TestWriteTicketFailureIsReported(t *testing.T) {
	s := &session{cliState: "cli-state-1"}
	// 直接验证判定逻辑：/login 不是站点根。
	if isRootLocation("/login?force_login_type=&platform=CLI&state=x", "https://www.codebuddy.cn") {
		t.Fatal("/login must not count as success")
	}
	if !isRootLocation("/", "https://www.codebuddy.cn") {
		t.Fatal("/ must count as success")
	}
	if !isRootLocation("https://www.codebuddy.cn/", "https://www.codebuddy.cn") {
		t.Fatal("absolute root must count as success")
	}
	if isRootLocation("", "https://www.codebuddy.cn") {
		t.Fatal("empty location must not count as success")
	}
	_ = s
}

func TestSendRejectsGlobalRegion(t *testing.T) {
	m := NewManager(DefaultEndpoints(), 0)
	if _, err := m.Send(context.Background(), "+852 64087495", "global"); err != ErrGlobalUnsupported {
		t.Fatalf("err=%v want ErrGlobalUnsupported", err)
	}
}

// TestNormalizeMobile 文档记录香港号区号后必须有空格，连写会发码失败。
func TestNormalizeMobile(t *testing.T) {
	cases := map[string]string{
		"64087495":        "+86 64087495",
		"13800138000":     "+86 13800138000",
		"+86 13800138000": "+86 13800138000",
		"+852 64087495":   "+852 64087495",
		"+85264087495":    "+852 64087495",
		"85264087495":     "+852 64087495",
		"+853 66123456":   "+853 66123456",
		"+886 912345678":  "+886 912345678",
		" 138-0013-8000 ": "+86 13800138000",
	}
	for in, want := range cases {
		got, err := NormalizeMobile(in)
		if err != nil {
			t.Fatalf("NormalizeMobile(%q) err=%v", in, err)
		}
		if got != want {
			t.Errorf("NormalizeMobile(%q)=%q want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "123", "9999999999999999999"} {
		if _, err := NormalizeMobile(bad); err == nil {
			t.Errorf("NormalizeMobile(%q) should fail", bad)
		}
	}
}

// TestSessionExpiry 过期会话不可用，且 GC 会回收内存。
func TestSessionExpiry(t *testing.T) {
	f := newFakeUpstream()
	defer f.close()
	m := NewManager(f.endpoints(), time.Minute)

	now := time.Now()
	m.now = func() time.Time { return now }

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	now = now.Add(2 * time.Minute)
	if _, err := m.Verify(context.Background(), send.SessionID, "123456"); err != ErrSessionNotFound {
		t.Fatalf("expired session err=%v want ErrSessionNotFound", err)
	}
	m.mu.Lock()
	left := len(m.sessions)
	m.mu.Unlock()
	if left != 0 {
		t.Fatalf("expired sessions should be collected, left=%d", left)
	}
}

// TestSendDoesNotLeakTokenIntoResult 回执只含 UI 需要的字段。
func TestSendDoesNotLeakTokenIntoResult(t *testing.T) {
	f := newFakeUpstream()
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	raw, err := json.Marshal(send)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "tok-send") {
		t.Fatalf("send result leaks the state_token: %s", raw)
	}
}

// TestBrokerLoginURLIsRefetched 每次 Verify 都重新拉登录页，避免 session_code 过期。
func TestBrokerLoginURLIsRefetched(t *testing.T) {
	f := newFakeUpstream()
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	for i := 0; i < 2; i++ {
		send, err := m.Send(context.Background(), "+852 64087495", "cn")
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		if _, err := m.Verify(context.Background(), send.SessionID, "123456"); err != nil {
			t.Fatalf("verify %d: %v", i, err)
		}
	}
	authCalls := 0
	for _, c := range f.callList() {
		if strings.Contains(c, "protocol/openid-connect/auth") {
			authCalls++
		}
	}
	if authCalls != 2 {
		t.Fatalf("keycloak login page fetched %d times, want 2", authCalls)
	}
}

func TestRedact(t *testing.T) {
	long := strings.Repeat("a", 40)
	msg := "E0010072 无效的token" + long
	got := redact(msg)
	if strings.Contains(got, long) {
		t.Fatalf("redact left the token: %q", got)
	}
	if !strings.Contains(got, "E0010072") {
		t.Fatalf("redact removed the error code: %q", got)
	}
}

var _ = url.Values{}
