package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/smslogin"
)

// fakeSMSUpstream 是 server 包测试用的假上游：复刻短信登录链路的三个主机，
// 让 handler 层可以在无网络、无真实短信的前提下端到端跑通。
// 与 internal/smslogin 包内的测试桩相互独立（那个不可跨包复用）。
type fakeSMSUpstream struct {
	console *httptest.Server
	cli     *httptest.Server
	oneID   *httptest.Server

	// mu 保护下面两个字段：handler 测试会并发打请求。
	mu sync.Mutex
	// sentToken 是最近一次 send 签发的 token。真实上游每次 send 换发新 token，
	// 重发会让旧 token 立刻失效。
	sentToken string
	// verified 记录已通过验码的 token，accounts 只认这些。
	verified map[string]bool
	// sendSeq 保证每次 send 的 token 都不同。
	sendSeq int
	// needCaptcha 模拟上游要求人机校验（实测 captcha 是数组）。
	needCaptcha bool
	// captchaOnlyTeg 模拟上游只给 teg（2captcha 不支持的类型）。
	captchaOnlyTeg bool

	// captchaVerification 记录最近一次回灌的 captchaVerification。
	captchaVerification map[string]any
}

// LastCaptchaVerification 返回最近一次收到的 captchaVerification，供断言形状。
func (f *fakeSMSUpstream) LastCaptchaVerification() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.captchaVerification
}

func newFakeSMSUpstream(t *testing.T) *fakeSMSUpstream {
	t.Helper()
	f := &fakeSMSUpstream{verified: map[string]bool{}}

	f.cli = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			smsEnvelope(w, map[string]any{
				"state":   "cli-state-1",
				"authUrl": f.console.URL + "/login/?platform=CLI&state=cli-state-1",
			})
		case "/v2/plugin/auth/token":
			smsEnvelope(w, map[string]any{
				"accessToken": "sms-at-1", "refreshToken": "sms-rt-1",
				"expiresIn": 5184000, "domain": "www.codebuddy.cn",
			})
		case "/v2/plugin/login/account":
			if r.Header.Get("Authorization") != "Bearer sms-at-1" {
				smsRaw(w, 401, `{"code":401,"msg":"unauthorized"}`)
				return
			}
			smsEnvelope(w, map[string]any{"uid": "uid-sms-1", "enterpriseId": "", "nickname": "64087495"})
		default:
			smsRaw(w, 404, `{"code":404,"msg":"not found"}`)
		}
	}))

	f.oneID = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/sms/code/send":
			var req struct {
				ClientCode string `json:"client_code"`
				Mobile     string `json:"mobile"`
				// 过码后回灌的字段，形状为对象 {ticket, randStr, cloudType}。
				CaptchaVerification map[string]any `json:"captchaVerification"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.ClientCode != "codebuddy" || !strings.Contains(req.Mobile, " ") {
				smsRaw(w, 400, `{"errCode":"E0010343","errMessage":"参数错误"}`)
				return
			}
			// 要求人机校验：没有 captchaVerification 就先给挑战（实测是数组）。
			if f.needCaptcha && req.CaptchaVerification == nil {
				body := `{"captcha":[{"appId":"2053989439","cloudType":"teg"},{"appId":"197561220","cloudType":"tencent"}],"expires_in":0,"status":"need_captcha","state_token":""}`
				if f.captchaOnlyTeg {
					body = `{"captcha":[{"appId":"2053989439","cloudType":"teg"}],"expires_in":0,"status":"need_captcha","state_token":""}`
				}
				smsRaw(w, 200, body)
				return
			}
			// 记下回灌内容，供断言"人工票据确实被送到上游"。
			f.mu.Lock()
			f.captchaVerification = req.CaptchaVerification
			f.mu.Unlock()
			// 每次 send 换发新 token，旧 token 随之失效。
			f.mu.Lock()
			f.sendSeq++
			f.sentToken = fmt.Sprintf("tok-sms-send-%d", f.sendSeq)
			token := f.sentToken
			f.verified = map[string]bool{}
			f.mu.Unlock()
			// 与真实上游一致：不需要验证码时 captcha 是字面量 null。
			smsRaw(w, 200, fmt.Sprintf(`{"captcha":null,"expires_in":300,"status":"unexpired","state_token":%q}`, token))
		case "/v1/auth/sms/code/verify":
			var req struct {
				StateToken string `json:"state_token"`
				Code       string `json:"code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			live := req.StateToken != "" && req.StateToken == f.sentToken
			f.mu.Unlock()
			if !live {
				// 真实 OneID 会把 token 回显进错误消息。
				smsRaw(w, 400, fmt.Sprintf(`{"errCode":"E0010072","errMessage":"无效的token%s"}`, req.StateToken))
				return
			}
			if req.Code != "123456" {
				// 真实上游：验证码错误不会作废 state_token，可直接重填。
				smsRaw(w, 400, `{"errCode":"E0010028","errMessage":"验证码错误，请重新填写"}`)
				return
			}
			next := req.StateToken + "-verified"
			f.mu.Lock()
			f.verified[next] = true
			f.mu.Unlock()
			smsRaw(w, 200, fmt.Sprintf(`{"state_token":%q}`, next))
		case "/v1/auth/accounts":
			tok := r.URL.Query().Get("state_token")
			f.mu.Lock()
			ok := f.verified[tok]
			f.mu.Unlock()
			if !ok {
				smsRaw(w, 400, `{"errCode":"E0010072","errMessage":"无效的token"}`)
				return
			}
			smsRaw(w, 200, `{"accounts":[{"id":"codebuddy@","name":"64087495","type":2}],"code":"oneid-code-1","next_step":"login"}`)
		default:
			smsRaw(w, 404, `{"errCode":"E404","errMessage":"not found"}`)
		}
	}))

	f.console = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/realms/copilot/protocol/openid-connect/auth":
			w.Header().Add("Set-Cookie", `AUTH_SESSION_ID=s1; Version=1; Path="/auth/realms/copilot"; HttpOnly`)
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprintf(w, `<a data-idp="oneid" href="%s/auth/realms/copilot/broker/oneid/login?client_id=console&amp;tab_id=t1&amp;session_code=sc1">OneID</a>`, f.console.URL)
		case "/auth/realms/copilot/broker/oneid/login":
			raw, _ := json.Marshal(map[string]any{
				"code":         0,
				"state":        "broker-state-1",
				"redirect_uri": f.console.URL + "/auth/realms/copilot/broker/oneid/endpoint",
			})
			smsRaw(w, 200, string(raw))
		case "/auth/realms/copilot/broker/oneid/endpoint":
			if r.URL.Query().Get("code") != "oneid-code-1" {
				smsRaw(w, 400, `{"code":400,"msg":"bad code"}`)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "KEYCLOAK_IDENTITY", Value: "kc1", Path: "/auth/realms/copilot"})
			w.Header().Set("Location", "/login/?platform=CLI&state=cli-state-1")
			w.WriteHeader(302)
		case "/login/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html>login</html>`))
		case "/console/accounts":
			if c, err := r.Cookie("session"); err != nil || c.Value != "apisix-1" {
				http.SetCookie(w, &http.Cookie{Name: "session", Value: "apisix-1", Path: "/"})
				w.Header().Set("Location", "/console/accounts")
				w.WriteHeader(302)
				return
			}
			smsRaw(w, 200, `[{"nickname":"64087495","pluginEnabled":true}]`)
		case "/console/login/enterprise":
			smsEnvelope(w, map[string]any{"accessToken": "console-at-1", "tokenType": "Bearer"})
		case "/v2/plugin/auth/token":
			smsEnvelope(w, map[string]any{
				"accessToken": "sms-at-1", "refreshToken": "sms-rt-1",
				"expiresIn": 5184000, "domain": "www.codebuddy.cn",
			})
		case "/console/auth/login":
			// 实测真实行为：带 Authorization Bearer 得 302 Location=/ 假成功；
			// 不带 Bearer（纯 APISIX session）才真正写上，Location 是 /login?...。
			if r.Header.Get("Authorization") != "" {
				w.Header().Set("Location", "/")
				w.WriteHeader(302)
				return
			}
			w.Header().Set("Location", "/login?platform=CLI&state=cli-state-1")
			w.WriteHeader(302)
		default:
			smsRaw(w, 404, `{"code":404,"msg":"not found"}`)
		}
	}))

	return f
}

func (f *fakeSMSUpstream) Close() {
	if f.console != nil {
		f.console.Close()
	}
	if f.cli != nil {
		f.cli.Close()
	}
	if f.oneID != nil {
		f.oneID.Close()
	}
}

func (f *fakeSMSUpstream) Endpoints() smslogin.Endpoints {
	return smslogin.Endpoints{Console: f.console.URL, CLI: f.cli.URL, OneID: f.oneID.URL, Realm: "copilot"}
}

func smsEnvelope(w http.ResponseWriter, data any) {
	raw, _ := json.Marshal(map[string]any{"code": 0, "msg": "OK", "data": data})
	smsRaw(w, 200, string(raw))
}

func smsRaw(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
