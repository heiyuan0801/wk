package smslogin

import (
	"context"
	"encoding/json"
	"errors"
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
	// needCaptcha 模拟上游要求图形验证码。
	needCaptcha bool
	// captchaOnlyTeg 模拟上游只给 teg（2captcha 不支持的类型）。
	captchaOnlyTeg bool
	// alwaysChallenge 表示即使带了 captchaVerification 也仍要挑战（票据过期）。
	alwaysChallenge bool
	// lastCaptchaVerification 记录回灌的 captchaVerification，供断言形状。
	lastCaptchaVerification map[string]any
	// solvedCaptcha 置位表示已经过码，下一次 send 应放行。
	solvedCaptcha bool
	// tooFrequent 模拟上游频控（E0010022）。
	tooFrequent bool
	// sentTokens 是 send 签发的 state_token。真实上游每次 send 都换发新 token，
	// 旧 token 随即失效——重发语义依赖这一点。
	sentTokens []string
	// verifiedTokens 是通过验码的 token，accounts 只认这些。
	verifiedTokens []string
	// sendSeq 让每次 send 的 token 都不同。
	sendSeq int
	// tokenHits 是取票接口被打了几次。pendingUntil 次 11217 之后才出票，
	// 用来证明 fetchTicket 会短轮询，而不是第一次 11217 就放弃。
	tokenHits    int
	pendingUntil int
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
			f.mu.Lock()
			f.tokenHits++
			hits := f.tokenHits
			need := f.pendingUntil
			ready := f.pendingTicket && hits > need
			f.mu.Unlock()
			if !ready {
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
				// 过码后回灌的字段，形状为对象 {ticket, randStr, cloudType}。
				CaptchaVerification map[string]any `json:"captchaVerification"`
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
			// 真实上游在不要求验证码时回的是字面量 "captcha":null。
			if f.needCaptcha {
				f.mu.Lock()
				f.lastCaptchaVerification = req.CaptchaVerification
				f.mu.Unlock()
				if req.CaptchaVerification == nil || f.alwaysChallenge {
					// 实测形状是数组：teg 在前、tencent 在后。
					body := `{"captcha":[{"appId":"2053989439","cloudType":"teg"},{"appId":"197561220","cloudType":"tencent"}],"expires_in":0,"status":"need_captcha","state_token":""}`
					if f.captchaOnlyTeg {
						body = `{"captcha":[{"appId":"2053989439","cloudType":"teg"}],"expires_in":0,"status":"need_captcha","state_token":""}`
					}
					writeJSONRaw(w, 200, body)
					return
				}
				// 带上 captchaVerification 后应放行，变成 unexpired。
			}
			if f.tooFrequent {
				writeJSONRaw(w, 406, `{"errCode":"E0010022","errMessage":"操作太频繁，请稍后重试"}`)
				return
			}
			// 每次 send 换发新 token，并作废此前所有未使用的 token。
			f.mu.Lock()
			f.sendSeq++
			token := fmt.Sprintf("tok-send-%d", f.sendSeq)
			f.sentTokens = []string{token}
			f.verifiedTokens = nil
			f.mu.Unlock()
			writeJSONRaw(w, 200, fmt.Sprintf(
				`{"captcha":null,"expires_in":300,"status":"unexpired","state_token":%q}`, token))
		case "/v1/auth/sms/code/verify":
			var req struct {
				StateToken string `json:"state_token"`
				Code       string `json:"code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			live := false
			for _, tok := range f.sentTokens {
				if tok == req.StateToken {
					live = true
				}
			}
			f.mu.Unlock()
			if !live {
				// token 已随重发作废。
				writeJSONRaw(w, 400, fmt.Sprintf(`{"errCode":"E0010072","errMessage":"无效的token%s"}`, req.StateToken))
				return
			}
			if f.tooFrequent {
				writeJSONRaw(w, 406, `{"errCode":"E0010022","errMessage":"操作太频繁，请稍后重试"}`)
				return
			}
			if req.Code != "123456" {
				// 真实上游：验证码错误时 state_token 仍然有效，可以直接重填。
				writeJSONRaw(w, 400, `{"errCode":"E0010028","errMessage":"验证码错误，请重新填写"}`)
				return
			}
			next := req.StateToken + "-verified"
			f.mu.Lock()
			f.verifiedTokens = append(f.verifiedTokens, next)
			f.mu.Unlock()
			writeJSONRaw(w, 200, fmt.Sprintf(`{"state_token":%q}`, next))
		case "/v1/auth/accounts":
			tok := r.URL.Query().Get("state_token")
			f.mu.Lock()
			ok := false
			for _, v := range f.verifiedTokens {
				if v == tok {
					ok = true
				}
			}
			f.mu.Unlock()
			if !ok {
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
			// 真实 Keycloak 用 RFC2109：Version=1 且 Path 带引号。Go 标准解析
			// 会丢掉 Path，后续 first-broker-login 就带不上会话 Cookie。
			w.Header().Add("Set-Cookie", `AUTH_SESSION_ID=auth-session-1; Version=1; Path="/auth/realms/copilot"; HttpOnly`)
			w.Header().Set("Content-Type", "text/html")
			// href 里的 & 以 &amp; 形式出现，正是真实页面的形态。
			_, _ = fmt.Fprintf(w, `<a data-idp="oneid" href="%s/auth/realms/copilot/broker/oneid/login?client_id=console&amp;tab_id=t1&amp;session_code=sc1">OneID</a>`,
				f.consoleURL())
		case r.URL.Path == "/auth/realms/copilot/broker/oneid/login":
			if r.URL.Query().Get("from_oneid_login") != "true" {
				writeJSONRaw(w, 400, `{"code":400,"msg":"missing from_oneid_login"}`)
				return
			}
			// 真实 Keycloak 把 state / redirect_uri 放在顶层，没有 data 信封。
			writeJSONRaw(w, 200, mustJSON(map[string]any{
				"code":         0,
				"state":        "broker-state-1",
				"redirect_uri": f.consoleURL() + "/auth/realms/copilot/broker/oneid/endpoint",
			}))
		case r.URL.Path == "/auth/realms/copilot/broker/oneid/endpoint":
			if r.URL.Query().Get("code") != "oneid-code-1" || r.URL.Query().Get("state") != "broker-state-1" {
				writeJSONRaw(w, 400, `{"code":400,"msg":"bad broker callback"}`)
				return
			}
			if _, err := r.Cookie("AUTH_SESSION_ID"); err != nil {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("missing AUTH_SESSION_ID"))
				return
			}
			// 真实环境会连跳 first-broker-login → after-first-broker-login → /login。
			// 文档 curl -L 不带 Referer；Keycloak 页是 referrer-policy: no-referrer。
			w.Header().Set("Location", "/auth/realms/copilot/login-actions/first-broker-login?client_id=console")
			w.WriteHeader(302)
		case r.URL.Path == "/auth/realms/copilot/login-actions/first-broker-login":
			if _, err := r.Cookie("AUTH_SESSION_ID"); err != nil {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`Tencent Cloud CodeBuddy checkCookiesAndSetTimeout`))
				return
			}
			if r.Header.Get("Referer") != "" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("referer not allowed on keycloak login-actions"))
				return
			}
			if acc := r.Header.Get("Accept"); strings.Contains(acc, "application/json") || r.Header.Get("Origin") != "" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("xhr headers forbidden"))
				return
			}
			// 网关会把打到 /auth 的 X-Domain 直接 403。
			if r.Header.Get("X-Domain") != "" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("x-domain not allowed on keycloak"))
				return
			}
			setCookie(w, "KEYCLOAK_IDENTITY", "kc-identity-1", "/auth/realms/copilot")
			w.Header().Set("Location", "/login/?platform=CLI&state=cli-state-1&code=kc-code")
			w.WriteHeader(302)
		case r.URL.Path == "/login/":
			// 真实环境落点有时是 HTML 200，有时被 WAF/页面本身 403。
			// 只要前面已经种下 KEYCLOAK_IDENTITY，403 不能当成登录失败。
			if acc := r.Header.Get("Accept"); strings.Contains(acc, "application/json") {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("json accept forbidden"))
				return
			}
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusForbidden)
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
		case r.URL.Path == "/v2/plugin/auth/token":
			// 生产里写票和取票都在 www.codebuddy.cn。测试里这条路径与 CLI 共用出票逻辑。
			f.mu.Lock()
			f.tokenHits++
			hits := f.tokenHits
			need := f.pendingUntil
			ready := f.pendingTicket && hits > need
			f.mu.Unlock()
			if !ready {
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
		case r.URL.Path == "/console/auth/login":
			// 真实上游（实测）：带 Authorization Bearer 会得到 302 Location=/
			// 的假成功，票实际没写上，取票永远 11217。不带 Bearer、纯 APISIX
			// session 时 302 到 /login?force_login_type=... 才是真的写上了。
			if r.Header.Get("Authorization") != "" {
				f.mu.Lock()
				f.pendingTicket = false
				f.mu.Unlock()
				w.Header().Set("Location", "/")
				w.WriteHeader(302)
				return
			}
			if !f.hasAPISIXSession(r) {
				w.Header().Set("Location", "/")
				w.WriteHeader(302)
				return
			}
			f.mu.Lock()
			f.pendingTicket = true
			f.ticketWritten = true
			f.mu.Unlock()
			// 302 到 /login?force... = 写票成功。
			w.Header().Set("Location", "/login?force_login_type=&platform=CLI&state=cli-state-1")
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
	m.sleep = func(context.Context, time.Duration) error { return nil }
	f.mu.Lock()
	f.pendingUntil = 2
	f.mu.Unlock()

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
	f.mu.Lock()
	hits := f.tokenHits
	f.mu.Unlock()
	if hits < 3 {
		t.Fatalf("expected ticket polling after 11217, tokenHits=%d", hits)
	}
	if !f.wasWritten() {
		t.Fatal("expected the one-time ticket to be written")
	}

	// 成功后会话应被丢掉，空码复用必须失败。
	if _, err := m.Verify(context.Background(), send.SessionID, ""); err == nil {
		t.Fatal("successful login must consume the session")
	}

	// 按文档顺序走完全部步骤。重定向落点（/login/ 与第二次 /console/accounts）
	// 也在其中：它们证明 broker 链与 APISIX OIDC 都真的被跟随了。
	got := f.callList()
	mustContainInOrder(t, got, []string{
		"cli /v2/plugin/auth/state",
		"oneid /v1/auth/sms/code/send",
		"oneid /v1/auth/sms/code/verify",
		"oneid /v1/auth/accounts",
		"console /auth/realms/copilot/protocol/openid-connect/auth",
		"console /auth/realms/copilot/broker/oneid/login",
		"console /auth/realms/copilot/broker/oneid/endpoint",
		"console /auth/realms/copilot/login-actions/first-broker-login",
		"console /login/",
		"console /console/accounts",
		"console /console/login/enterprise",
		"console /console/auth/login",
		"cli /v2/plugin/auth/token",
		"cli /v2/plugin/login/account",
	})
}

func mustContainInOrder(t *testing.T, got, want []string) {
	t.Helper()
	i := 0
	for _, step := range want {
		found := false
		for i < len(got) {
			if got[i] == step {
				found = true
				i++
				break
			}
			i++
		}
		if !found {
			t.Fatalf("missing step %q in %v", step, got)
		}
	}
}

// TestVerifyWrongCodeKeepsSession 验证码填错时上游不会作废 state_token
// （E0010028），所以会话必须保留，让用户直接重填而不必浪费一条短信。
func TestVerifyWrongCodeKeepsSession(t *testing.T) {
	f := newFakeUpstream()
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	_, err = m.Verify(context.Background(), send.SessionID, "000000")
	if err == nil {
		t.Fatal("wrong code must fail")
	}
	var ue *Error
	if !errors.As(err, &ue) || !ue.Retryable {
		t.Fatalf("wrong code should be retryable, got %#v", err)
	}
	if !errors.Is(err, ErrCodeWrong) {
		t.Fatalf("err should wrap ErrCodeWrong: %v", err)
	}
	// 同一 session 直接重填正确验证码必须成功。
	if _, err := m.Verify(context.Background(), send.SessionID, "123456"); err != nil {
		t.Fatalf("session must survive a wrong code: %v", err)
	}
}

// TestVerifySuccessConsumesSession 验码通过后中间凭据即为一次性，必须丢弃。
func TestVerifySuccessConsumesSession(t *testing.T) {
	f := newFakeUpstream()
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := m.Verify(context.Background(), send.SessionID, "123456"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := m.Verify(context.Background(), send.SessionID, "123456"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("session must be consumed after success, err=%v", err)
	}
}

// TestResendIssuesNewTokenAndInvalidatesOld 重发会换发新 state_token，
// 旧 session 必须失效，否则用户会拿着死 token 反复报错。
func TestResendIssuesNewTokenAndInvalidatesOld(t *testing.T) {
	f := newFakeUpstream()
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	first, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	second, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	if first.SessionID == second.SessionID {
		t.Fatal("resend must mint a new session")
	}
	if _, err := m.Verify(context.Background(), first.SessionID, "123456"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("old session must be invalidated by resend, err=%v", err)
	}
	if _, err := m.Verify(context.Background(), second.SessionID, "123456"); err != nil {
		t.Fatalf("new session must work: %v", err)
	}
}

// TestTooFrequentIsRetryable 频控（E0010022）不破坏会话，稍后重试即可。
func TestTooFrequentIsRetryable(t *testing.T) {
	f := newFakeUpstream()
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	f.tooFrequent = true
	_, err = m.Verify(context.Background(), send.SessionID, "123456")
	if err == nil {
		t.Fatal("frequency control must fail")
	}
	var ue *Error
	if !errors.As(err, &ue) || !ue.Retryable {
		t.Fatalf("frequency control should be retryable, got %#v", err)
	}
	if !errors.Is(err, ErrTooFrequent) {
		t.Fatalf("err should wrap ErrTooFrequent: %v", err)
	}
	// 频控过后同一 session 仍可用。
	f.tooFrequent = false
	if _, err := m.Verify(context.Background(), send.SessionID, "123456"); err != nil {
		t.Fatalf("session must survive frequency control: %v", err)
	}
}

// TestSendTooFrequentKeepsOldCodeUsable 发码遇频控时应提示等待，而不是让用户
// 以为验证码作废了。
func TestSendTooFrequentKeepsOldCodeUsable(t *testing.T) {
	f := newFakeUpstream()
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	f.tooFrequent = true
	_, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err == nil {
		t.Fatal("send should fail while rate limited")
	}
	if !errors.Is(err, ErrTooFrequent) {
		t.Fatalf("err should wrap ErrTooFrequent: %v", err)
	}
}

// TestVerifyUnknownSession 未知/空 session 必须明确报 ErrSessionNotFound。
func TestVerifyUnknownSession(t *testing.T) {
	m := NewManager(DefaultEndpoints(), 0)
	if _, err := m.Verify(context.Background(), "nope", "123456"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err=%v want ErrSessionNotFound", err)
	}
	if _, err := m.Verify(context.Background(), "", "123456"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("empty session err=%v want ErrSessionNotFound", err)
	}
}

// TestConcurrentVerifyRejected 同一会话并发验码会被上游 406，必须提前拦下。
func TestConcurrentVerifyRejected(t *testing.T) {
	f := newFakeUpstream()
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	sess, err := m.claim(send.SessionID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	_ = sess
	if _, err := m.claim(send.SessionID); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("second claim err=%v want ErrSessionBusy", err)
	}
	m.release(send.SessionID, true)
	if _, err := m.claim(send.SessionID); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
}

// TestErrorRedactsToken 上游会把 state_token 回显进错误消息（如 E0010072
// "无效的token<值>"），向上传播前必须抹掉，不能把中间凭据送到浏览器。
func TestErrorRedactsToken(t *testing.T) {
	f := newFakeUpstream()
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	if _, err := m.Send(context.Background(), "+852 64087495", "cn"); err != nil {
		t.Fatalf("first send: %v", err)
	}
	// 重发会让第一个 token 变成"无效"，正是上游回显 token 的分支。
	if _, err := m.Send(context.Background(), "+852 64087495", "cn"); err != nil {
		t.Fatalf("resend: %v", err)
	}
	// 旧会话已被重发清除，这里直接构造一个持有死 token 的会话走验码。
	sess, err := m.newSession("+852 64087495")
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	sess.oneIDTok = "dead-token-echoed-by-upstream"
	m.store(sess)

	_, err = m.Verify(context.Background(), sess.id, "999999")
	if err == nil {
		t.Fatal("expected failure")
	}
	if strings.Contains(err.Error(), "dead-token-echoed-by-upstream") {
		t.Fatalf("error leaks state_token: %v", err)
	}
	if !strings.Contains(err.Error(), "E0010072") {
		t.Fatalf("error should keep the oneid error code: %v", err)
	}
}

// TestWrongCodeErrorDoesNotEchoToken 验证码错误的提示必须干净：既不带 token，
// 也要让用户看得懂下一步该做什么。
func TestWrongCodeErrorDoesNotEchoToken(t *testing.T) {
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
	if !strings.Contains(err.Error(), "重新填写") {
		t.Fatalf("error should tell the user to re-enter the code: %v", err)
	}
}

// TestTicketPendingKeepsSessionForEmptyCodeRetry 写票后 11217 必须保留会话，
// 空码重试只重跑选账号/写票/取票，不再要求短信验证码。
func TestTicketPendingKeepsSessionForEmptyCodeRetry(t *testing.T) {
	f := newFakeUpstream()
	defer f.close()
	m := NewManager(f.endpoints(), 0)
	m.sleep = func(context.Context, time.Duration) error { return nil }
	f.mu.Lock()
	f.pendingUntil = 100
	f.mu.Unlock()

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	_, err = m.Verify(context.Background(), send.SessionID, "123456")
	if err == nil {
		t.Fatal("expected 11217")
	}
	if !strings.Contains(err.Error(), "可复用当前会话") {
		t.Fatalf("expected reusable session error, got %v", err)
	}

	f.mu.Lock()
	f.pendingUntil = 0
	f.pendingTicket = true
	f.mu.Unlock()
	creds, err := m.Verify(context.Background(), send.SessionID, "")
	if err != nil {
		t.Fatalf("empty-code retry: %v", err)
	}
	if creds.UID != "uid-1" || creds.AccessToken != "at-1" {
		t.Fatalf("credentials=%+v", creds)
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
func TestIsKeycloakPath(t *testing.T) {
	if !isKeycloakPath("https://www.codebuddy.cn/auth/realms/copilot/login-actions/first-broker-login?x=1") {
		t.Fatal("login-actions is keycloak")
	}
	if isKeycloakPath("https://www.codebuddy.cn/console/accounts") {
		t.Fatal("console is not keycloak")
	}
}

func TestIsConsoleAPIPath(t *testing.T) {
	if !isConsoleAPIPath("https://www.codebuddy.cn/console/accounts") {
		t.Fatal("console accounts should need X-Domain")
	}
	if isConsoleAPIPath("https://www.codebuddy.cn/auth/realms/copilot/login-actions/first-broker-login?x=1") {
		t.Fatal("keycloak path must not get X-Domain")
	}
	if isConsoleAPIPath("https://www.codebuddy.cn/login/?platform=CLI") {
		t.Fatal("login page must not get X-Domain")
	}
}

func TestHTMLErrorSnippetStripsTagsAndRedacts(t *testing.T) {
	got := htmlErrorSnippet([]byte(`<html><title>Forbidden</title><body>AUTH_SESSION_ID=abcdefghijklmnopqrstuvwxyz0123 missing</body></html>`))
	if !strings.Contains(got, "Forbidden") {
		t.Fatalf("snippet=%q", got)
	}
	if strings.Contains(got, "abcdefghijklmnopqrstuvwxyz0123") {
		t.Fatalf("must redact token-like strings: %q", got)
	}
}

func TestWriteTicketFailureIsReported(t *testing.T) {
	// 直接验证判定逻辑（实测：/login?force... 才是写上，站点根 / 是假成功）。
	if !isTicketWrittenLocation("/login?force_login_type=&platform=CLI&state=x") {
		t.Fatal("/login?force... must count as written")
	}
	if !isTicketWrittenLocation("https://www.codebuddy.cn/login?force_login_type=") {
		t.Fatal("absolute /login must count as written")
	}
	if isTicketWrittenLocation("/") {
		t.Fatal("root must NOT count as written (bearer fake success)")
	}
	if isTicketWrittenLocation("") {
		t.Fatal("empty location must not count as written")
	}
	if isTicketWrittenLocation("/console/foo") {
		t.Fatal("other paths must not count as written")
	}
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

// TestSendAcceptsNullCaptcha 上游在不需要图形验证码时回的是 `"captcha":null`。
// json.RawMessage 对 null 的长度是 4，早期实现用 len(...) > 0 判空，把所有正常
// 发码响应都误判成"需要图形验证码"，导致功能整体不可用。此用例锁住该回归。
func TestSendAcceptsNullCaptcha(t *testing.T) {
	f := newFakeUpstream()
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("null captcha must not be treated as a challenge: %v", err)
	}
	if send.SessionID == "" {
		t.Fatal("send should return a session id")
	}
	// 拿到会话后必须能继续走完验码。
	if _, err := m.Verify(context.Background(), send.SessionID, "123456"); err != nil {
		t.Fatalf("verify after null captcha: %v", err)
	}
}

// TestSendCaptchaWithoutSolverFallsBackToManual 未配置打码平台时不再直接失败：
// 会话要保留，并把挑战交给调用方让用户手动完成。这是 teg 类型的唯一出路，
// 也是没买打码服务时的默认路径。
func TestSendCaptchaWithoutSolverFallsBackToManual(t *testing.T) {
	f := newFakeUpstream()
	f.needCaptcha = true
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("captcha must not be a hard failure anymore: %v", err)
	}
	if send.Status != "need_captcha" {
		t.Fatalf("status=%q want need_captcha", send.Status)
	}
	if send.SessionID == "" {
		t.Fatal("session must be kept so the user can solve the captcha")
	}
	if send.Captcha == nil || len(send.Captcha.Options) != 2 {
		t.Fatalf("challenge must be handed to the caller: %+v", send.Captcha)
	}
	if send.Captcha.AutoAttempted {
		t.Fatal("no solver configured, so nothing was attempted automatically")
	}
	if !strings.Contains(send.Captcha.Reason, "手动") {
		t.Fatalf("reason should tell the user to solve it manually: %q", send.Captcha.Reason)
	}
	// 此时还没有 state_token，验码必须被明确挡回，而不是报一个语焉不详的上游错误。
	if _, err := m.Verify(context.Background(), send.SessionID, "123456"); !errors.Is(err, ErrCaptchaPending) {
		t.Fatalf("verify before solving should report ErrCaptchaPending, got %v", err)
	}
}

// TestCaptchaChallengeParsing 上游 captcha 字段出现过 null / 对象 / 数组三种形状
// （实测是数组）。解析必须全兼容，否则整条登录链路会因格式变化而中断。
func TestCaptchaChallengeParsing(t *testing.T) {
	// 实测形状：teg 在前、tencent 在后。
	live := `{"captcha":[{"appId":"2053989439","cloudType":"teg"},{"appId":"197561220","cloudType":"tencent"}],"expires_in":0,"status":"need_captcha","state_token":""}`
	var arr oneIDSendResponse
	if err := json.Unmarshal([]byte(live), &arr); err != nil {
		t.Fatalf("array form: %v", err)
	}
	if len(arr.Captcha) != 2 {
		t.Fatalf("expected 2 options, got %d", len(arr.Captcha))
	}
	opt, ok := arr.Captcha.PickSolvable()
	if !ok {
		t.Fatal("must find a solvable option")
	}
	// 必须跳过 teg 选中 tencent，并带上它自己的 appId。
	if opt.CloudType != "tencent" || opt.AppID != "197561220" {
		t.Fatalf("picked wrong option: %+v", opt)
	}

	// 单对象形式。
	var one oneIDSendResponse
	if err := json.Unmarshal([]byte(`{"captcha":{"appId":"197561220","cloudType":"tencent"}}`), &one); err != nil {
		t.Fatalf("object form: %v", err)
	}
	if len(one.Captcha) != 1 {
		t.Fatalf("object form should yield 1 option, got %d", len(one.Captcha))
	}

	// null 与缺失都表示"不需要验证码"。
	for _, raw := range []string{`{"captcha":null}`, `{}`} {
		var none oneIDSendResponse
		if err := json.Unmarshal([]byte(raw), &none); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if len(none.Captcha) != 0 {
			t.Fatalf("%s should have no challenge, got %d", raw, len(none.Captcha))
		}
	}
}

// TestPickSolvableSkipsTegOnly 只有 teg 时无法自动过码，必须明确报不支持。
func TestPickSolvableSkipsTegOnly(t *testing.T) {
	c := CaptchaChallenge{{AppID: "2053989439", CloudType: "teg"}}
	if _, ok := c.PickSolvable(); ok {
		t.Fatal("teg alone must not be considered solvable")
	}
	// appId 为空也不能当成可用方案。
	c = CaptchaChallenge{{AppID: "", CloudType: "tencent"}}
	if _, ok := c.PickSolvable(); ok {
		t.Fatal("empty appId must not be considered solvable")
	}
}

// TestTegOnlyChallengeIsManual 上游只给 teg 时，2captcha 打不了，但用户可以手动过。
// 这正是手动打码存在的意义，所以不能当成硬失败。
func TestTegOnlyChallengeIsManual(t *testing.T) {
	f := newFakeUpstream()
	f.needCaptcha = true
	f.captchaOnlyTeg = true
	defer f.close()
	m := NewManager(f.endpoints(), 0)
	solver := &fakeSolver{ticket: "t", randStr: "r"}
	m.SetSolver(solver)

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("teg-only should fall back to manual, not fail: %v", err)
	}
	if send.Captcha == nil || send.Status != "need_captcha" {
		t.Fatalf("expected a manual challenge, got %+v", send)
	}
	// 根本不该去打扰打码平台。
	if solver.callCount != 0 {
		t.Fatalf("solver must not be called for teg, got %d calls", solver.callCount)
	}
	if !strings.Contains(send.Captcha.Reason, "手动") {
		t.Fatalf("reason=%q", send.Captcha.Reason)
	}
}

// TestSendSolvesCaptchaAndRetries need_captcha 时应自动过码并带上
// captchaVerification 重发，最终拿到 state_token。
func TestSendSolvesCaptchaAndRetries(t *testing.T) {
	f := newFakeUpstream()
	f.needCaptcha = true
	defer f.close()
	m := NewManager(f.endpoints(), 0)
	solver := &fakeSolver{ticket: "tr03_ticket", randStr: "@randstr"}
	m.SetSolver(solver)

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("send with captcha: %v", err)
	}
	if send.SessionID == "" {
		t.Fatal("should get a session after solving the captcha")
	}
	// solver 必须拿到 tencent 那条的 appId。
	if solver.gotAppID != "197561220" {
		t.Fatalf("solver appId=%q want 197561220", solver.gotAppID)
	}
	// 回灌字段形状：对象 + 大写 S 的 randStr + 与所用 appId 一致的 cloudType。
	got := f.lastCaptchaVerification
	if got == nil {
		t.Fatal("captchaVerification must be sent back")
	}
	if got["ticket"] != "tr03_ticket" || got["randStr"] != "@randstr" {
		t.Fatalf("captchaVerification=%v", got)
	}
	if got["cloudType"] != "tencent" {
		t.Fatalf("cloudType must match the used appId entry: %v", got["cloudType"])
	}
	// 过码后链路必须能继续走到验码。
	if _, err := m.Verify(context.Background(), send.SessionID, "123456"); err != nil {
		t.Fatalf("verify after captcha: %v", err)
	}
}

// TestSendCaptchaSolverFailureFallsBackToManual 打码平台失败（如余额不足）
// 也要优雅降级到手动，而不是把用户堵死。
func TestSendCaptchaSolverFailureFallsBackToManual(t *testing.T) {
	f := newFakeUpstream()
	f.needCaptcha = true
	defer f.close()
	m := NewManager(f.endpoints(), 0)
	m.SetSolver(&fakeSolver{err: ErrCaptchaBalance})

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("solver failure should degrade to manual: %v", err)
	}
	if send.Captcha == nil {
		t.Fatal("challenge must still be handed over")
	}
	if !send.Captcha.AutoAttempted {
		t.Fatal("should record that auto solving was attempted")
	}
	if !strings.Contains(send.Captcha.Reason, "余额不足") {
		t.Fatalf("reason should name the real cause: %q", send.Captcha.Reason)
	}
}

// TestSubmitCaptchaSendsVerification 用户手动过码后提交，应带上 captchaVerification
// 继续发码，最终拿到 state_token 并可正常验码。
func TestSubmitCaptchaSendsVerification(t *testing.T) {
	f := newFakeUpstream()
	f.needCaptcha = true
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if send.Captcha == nil {
		t.Fatal("expected a manual challenge")
	}

	// 用小写字段名提交，同时 cloudType 必须原样回传上游给的那条。
	res, err := m.SubmitCaptcha(context.Background(), send.SessionID,
		"tr03_manual_ticket", "@manual_randstr", "tencent")
	if err != nil {
		t.Fatalf("submit captcha: %v", err)
	}
	if res.Status != "unexpired" || res.ExpiresIn == 0 {
		t.Fatalf("expected a live code, got %+v", res)
	}
	if res.Captcha != nil {
		t.Fatal("no challenge should remain after solving")
	}
	got := f.lastCaptchaVerification
	if got == nil {
		t.Fatal("captchaVerification must be sent upstream")
	}
	if got["ticket"] != "tr03_manual_ticket" || got["randStr"] != "@manual_randstr" {
		t.Fatalf("captchaVerification=%v", got)
	}
	if got["cloudType"] != "tencent" {
		t.Fatalf("cloudType must match the option used: %v", got["cloudType"])
	}
	// 过码后链路必须能继续走完。
	if _, err := m.Verify(context.Background(), res.SessionID, "123456"); err != nil {
		t.Fatalf("verify after manual captcha: %v", err)
	}
}

// TestSubmitCaptchaIncompleteIsItsOwnReason 结果不完整要报"不完整"，
// 不能借用打码平台密钥缺失的错误——那会让用户以为是配置问题。
func TestSubmitCaptchaIncompleteReason(t *testing.T) {
	f := newFakeUpstream()
	f.needCaptcha = true
	defer f.close()
	m := NewManager(f.endpoints(), 0)
	send, _ := m.Send(context.Background(), "+852 64087495", "cn")

	_, err := m.SubmitCaptcha(context.Background(), send.SessionID, "", "", "tencent")
	if !errors.Is(err, ErrCaptchaIncomplete) {
		t.Fatalf("err=%v want ErrCaptchaIncomplete", err)
	}
	if errors.Is(err, ErrCaptchaNoKey) {
		t.Fatal("incomplete result must not be reported as a missing captcha key")
	}
}

// TestSubmitCaptchaRejectsIncomplete 票据不完整时不能发请求。
func TestSubmitCaptchaRejectsIncomplete(t *testing.T) {
	f := newFakeUpstream()
	f.needCaptcha = true
	defer f.close()
	m := NewManager(f.endpoints(), 0)
	send, _ := m.Send(context.Background(), "+852 64087495", "cn")

	for _, tc := range []struct{ ticket, randStr string }{
		{"", "@r"}, {"t", ""}, {"", ""},
	} {
		if _, err := m.SubmitCaptcha(context.Background(), send.SessionID, tc.ticket, tc.randStr, "tencent"); err == nil {
			t.Fatalf("incomplete ticket %q/%q must be rejected", tc.ticket, tc.randStr)
		}
	}
}

// TestSubmitCaptchaExpiredTicketKeepsSession 票据过期时上游会再给一次挑战，
// 此时要保留会话并交出新的挑战，让用户重试而不是从头发短信。
func TestSubmitCaptchaExpiredTicketKeepsSession(t *testing.T) {
	f := newFakeUpstream()
	f.needCaptcha = true
	// 提交后仍然要挑战：模拟票据过期。
	f.alwaysChallenge = true
	defer f.close()
	m := NewManager(f.endpoints(), 0)

	send, _ := m.Send(context.Background(), "+852 64087495", "cn")
	res, err := m.SubmitCaptcha(context.Background(), send.SessionID, "stale", "@r", "tencent")
	if err != nil {
		t.Fatalf("expired ticket should not be a hard error: %v", err)
	}
	if res.Captcha == nil {
		t.Fatal("a fresh challenge must be handed back")
	}
	if res.SessionID != send.SessionID {
		t.Fatalf("session should be reused, got %q want %q", res.SessionID, send.SessionID)
	}
	if !strings.Contains(res.Captcha.Reason, "过期") {
		t.Fatalf("reason=%q", res.Captcha.Reason)
	}
	// 会话仍然可用：还能再提交一次。
	if _, err := m.SubmitCaptcha(context.Background(), send.SessionID, "t2", "@r2", "tencent"); err != nil {
		t.Fatalf("session must stay usable after an expired ticket: %v", err)
	}
}

// fakeSolver 记录被要求解的 appId，并按需返回票据或错误。
type fakeSolver struct {
	ticket    string
	randStr   string
	err       error
	gotAppID  string
	callCount int
}

func (s *fakeSolver) Solve(_ context.Context, appID string) (string, string, error) {
	s.gotAppID = appID
	s.callCount++
	if s.err != nil {
		return "", "", s.err
	}
	return s.ticket, s.randStr, nil
}

// TestNationalNumber 用于把手机号与账号 nickname 对齐：中国区账号的 nickname
// 就是不带区号的号码本身。必须能区分"手机号"与"普通昵称"，否则会误判已有账号。
func TestNationalNumber(t *testing.T) {
	cases := map[string]string{
		"+852 64087495":   "64087495",
		"+8613800138000":  "13800138000",
		"+86 13800138000": "13800138000",
		"13800138000":     "13800138000",
		"+853 66123456":   "66123456",
		"+886 912345678":  "912345678",
		// 无区号且以 86 开头时不能被误截：86 后面还有 9 位，视为号码本体。
		"8613800138000": "13800138000",
		// 太短或非号码一律返回空串，避免把普通昵称当成号码。
		"":         "",
		"12345":    "",
		"John Doe": "",
		"未命名":      "",
	}
	for in, want := range cases {
		if got := NationalNumber(in); got != want {
			t.Errorf("NationalNumber(%q)=%q want %q", in, got, want)
		}
	}
}

// TestDoEnvelopeAcceptsTopLevelBrokerFields 真实 Keycloak 把 state / redirect_uri
// 放在 {code:0} 的顶层，没有 data 信封。只读 data 会静默得到空字段。
func TestDoEnvelopeAcceptsTopLevelBrokerFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONRaw(w, 200, `{"code":0,"state":"kc-state","redirect_uri":"https://www.codebuddy.cn/auth/realms/copilot/broker/oneid/endpoint"}`)
	}))
	defer srv.Close()

	s := &session{consoleHTTP: srv.Client()}
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		State       string `json:"state"`
		RedirectURI string `json:"redirect_uri"`
	}
	if err := s.doEnvelope(s.consoleHTTP, req, &data); err != nil {
		t.Fatal(err)
	}
	if data.State != "kc-state" || !strings.Contains(data.RedirectURI, "broker/oneid/endpoint") {
		t.Fatalf("got %+v", data)
	}
}

func TestDoEnvelopeStillReadsNestedData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, map[string]any{"accessToken": "at-nested"})
	}))
	defer srv.Close()

	s := &session{consoleHTTP: srv.Client()}
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		AccessToken string `json:"accessToken"`
	}
	if err := s.doEnvelope(s.consoleHTTP, req, &data); err != nil {
		t.Fatal(err)
	}
	if data.AccessToken != "at-nested" {
		t.Fatalf("nested data lost: %+v", data)
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
