package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
}

func newFakeSMSUpstream(t *testing.T) *fakeSMSUpstream {
	t.Helper()
	f := &fakeSMSUpstream{}

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
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.ClientCode != "codebuddy" || !strings.Contains(req.Mobile, " ") {
				smsRaw(w, 400, `{"errCode":"E0010343","errMessage":"参数错误"}`)
				return
			}
			smsRaw(w, 200, `{"status":"unexpired","state_token":"tok-sms-send","expires_in":300}`)
		case "/v1/auth/sms/code/verify":
			var req struct {
				StateToken string `json:"state_token"`
				Code       string `json:"code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Code != "123456" {
				// 真实 OneID 会把 token 回显进错误消息。
				smsRaw(w, 400, fmt.Sprintf(`{"errCode":"E0010072","errMessage":"无效的token%s"}`, req.StateToken))
				return
			}
			smsRaw(w, 200, `{"state_token":"tok-sms-verify"}`)
		case "/v1/auth/accounts":
			if r.URL.Query().Get("state_token") != "tok-sms-verify" {
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
			http.SetCookie(w, &http.Cookie{Name: "AUTH_SESSION_ID", Value: "s1", Path: "/auth/realms/copilot"})
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprintf(w, `<a data-idp="oneid" href="%s/auth/realms/copilot/broker/oneid/login?client_id=console&amp;tab_id=t1&amp;session_code=sc1">OneID</a>`, f.console.URL)
		case "/auth/realms/copilot/broker/oneid/login":
			smsEnvelope(w, map[string]any{
				"state":        "broker-state-1",
				"redirect_uri": f.console.URL + "/auth/realms/copilot/broker/oneid/endpoint",
			})
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
		case "/console/auth/login":
			if r.Header.Get("Authorization") != "Bearer console-at-1" {
				w.Header().Set("Location", "/login?platform=CLI&state=cli-state-1")
				w.WriteHeader(302)
				return
			}
			w.Header().Set("Location", "/")
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
