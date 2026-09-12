package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/smslogin"
)

// smsTestEndpoints 指向一个假上游，让 handler 层的短信登录可以端到端跑通。
func smsTestHandler(t *testing.T, withManager bool) (*Handler, *pool.Pool, string) {
	t.Helper()
	up := newFakeSMSUpstream(t)
	t.Cleanup(up.Close)

	authDir := t.TempDir()
	p := testPoolWith()
	cfg := Config{Pool: p, AuthDir: authDir, Region: "cn"}
	if withManager {
		cfg.SMSLogin = smslogin.NewManager(up.Endpoints(), 0)
	}
	return NewHandler(cfg), p, authDir
}

func TestAdminSMSSendRequiresManager(t *testing.T) {
	h, _, _ := smsTestHandler(t, false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/send", strings.NewReader(`{"mobile":"+8613800138000"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestAdminSMSFullFlow 手机号 + 验证码即可添加账号：发码拿 session_id，
// 验码后 auths/ 落盘、账号进入池中。
func TestAdminSMSFullFlow(t *testing.T) {
	h, p, authDir := smsTestHandler(t, true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/send",
		strings.NewReader(`{"mobile":"+852 64087495","region":"cn"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("send code=%d body=%s", rec.Code, rec.Body)
	}
	var send struct {
		OK        bool   `json:"ok"`
		SessionID string `json:"session_id"`
		Mobile    string `json:"mobile"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &send); err != nil {
		t.Fatal(err)
	}
	if !send.OK || send.SessionID == "" {
		t.Fatalf("send response=%s", rec.Body)
	}
	// 会话 ID 是内部凭据，不能出现在响应之外；同时验证手机号已归一化。
	if send.Mobile != "+852 64087495" {
		t.Fatalf("mobile=%q", send.Mobile)
	}

	body := `{"session_id":"` + send.SessionID + `","code":"123456"}`
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/verify", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("verify code=%d body=%s", rec.Code, rec.Body)
	}
	var res struct {
		OK       bool   `json:"ok"`
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
		Region   string `json:"region"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.UID != "uid-sms-1" || res.Region != "cn" {
		t.Fatalf("verify response=%s", rec.Body)
	}

	// 落盘形状必须与 OAuth 路径一致，否则重启后加载不到账号。
	path := filepath.Join(authDir, "workbuddy-uid-sms-1.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("auth file: %v", err)
	}
	var doc struct {
		Auth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
		} `json:"auth"`
		Account struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Auth.AccessToken != "sms-at-1" || doc.Auth.RefreshToken != "sms-rt-1" {
		t.Fatalf("auth doc=%s", raw)
	}
	if doc.Auth.ExpiresAt <= 0 || doc.Auth.Domain != "www.codebuddy.cn" {
		t.Fatalf("auth doc=%s", raw)
	}
	if doc.Account.UID != "uid-sms-1" || doc.Account.Nickname != "64087495" {
		t.Fatalf("account doc=%s", raw)
	}
	// 文件权限必须是 0600：里面是长期 refresh token。
	// Windows 不实现 Unix 权限位（stat 一律报 0666），只有类 Unix 平台能验证。
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("auth file perm=%o want 600", perm)
		}
	}

	if got := p.AuthByUID("uid-sms-1"); got == nil {
		t.Fatal("account should be present in the pool")
	}
}

// TestAdminSMSVerifyWrongCode 验证码错误：不落盘，且会话被消费掉。
func TestAdminSMSVerifyWrongCode(t *testing.T) {
	h, p, authDir := smsTestHandler(t, true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/send",
		strings.NewReader(`{"mobile":"+8613800138000"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("send code=%d body=%s", rec.Code, rec.Body)
	}
	var send struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &send)

	body := `{"session_id":"` + send.SessionID + `","code":"000000"}`
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/verify", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong code=%d body=%s", rec.Code, rec.Body)
	}
	// 错误消息要能指导用户，但不能回显 state_token。
	if !strings.Contains(rec.Body.String(), "验证码") {
		t.Fatalf("body should mention the sms code: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "tok-sms-send") {
		t.Fatalf("body leaks state_token: %s", rec.Body)
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("no auth file should be written, got %d", len(entries))
	}
	if got := p.AuthByUID("uid-sms-1"); got != nil {
		t.Fatal("pool must not gain an account on failure")
	}
}

// TestAdminSMSExpiredSession 会话过期回 410，控制台据此提示重新发码。
func TestAdminSMSExpiredSession(t *testing.T) {
	h, _, _ := smsTestHandler(t, true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/verify",
		strings.NewReader(`{"session_id":"nonexistent","code":"123456"}`)))
	if rec.Code != http.StatusGone {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "重新发送验证码") {
		t.Fatalf("body should tell the user to resend: %s", rec.Body)
	}
}

// TestAdminSMSSendRejectsGlobalRegion 短信直登只覆盖中国区布局。
func TestAdminSMSSendRejectsGlobalRegion(t *testing.T) {
	h, _, _ := smsTestHandler(t, true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/send",
		strings.NewReader(`{"mobile":"+8613800138000","region":"global"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "仅支持中国区") {
		t.Fatalf("body=%s", rec.Body)
	}
}

// TestAdminSMSSendRejectsBadMobile 参数校验在发短信之前完成，避免白耗一条短信。
func TestAdminSMSSendRejectsBadMobile(t *testing.T) {
	h, _, _ := smsTestHandler(t, true)
	for _, body := range []string{
		`{"mobile":""}`,
		`{"mobile":"abc"}`,
		`{"mobile":"123"}`,
		`{`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/send", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%q code=%d resp=%s", body, rec.Code, rec.Body)
		}
	}
}

// TestPersistAccountRejectsPathTraversal UID 会拼进文件名，必须挡住路径穿越。
func TestPersistAccountRejectsPathTraversal(t *testing.T) {
	h, _, authDir := smsTestHandler(t, true)
	for _, uid := range []string{"../evil", "../../evil", "a/b", `a\b`, `..\evil`} {
		_, status, err := h.persistAccount(accountCredential{UID: uid, AccessToken: "x"}, "cn")
		if err == nil || status != http.StatusBadRequest {
			t.Fatalf("uid=%q should be rejected (status=%d err=%v)", uid, status, err)
		}
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("no file should be written, got %d", len(entries))
	}
	// 越权写入必须落在 auths/ 之外才算成功穿越，确认父目录未被污染。
	parent := filepath.Dir(authDir)
	siblings, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range siblings {
		if strings.Contains(e.Name(), "evil") {
			t.Fatalf("traversal wrote outside auths/: %s", e.Name())
		}
	}
}

// TestPersistAccountPromotesMixedRegion 加入 CN 账号时，海外池配置要提升为 all，
// 否则重启后新账号会消失。
func TestPersistAccountPromotesMixedRegion(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"region":"global"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	authDir := t.TempDir()
	p := testPoolWith(&auth.Auth{UID: "global-1", Domain: "www.workbuddy.ai"})
	h := NewHandler(Config{Pool: p, AuthDir: authDir, Region: "global", ConfigPath: cfgPath})

	res, status, err := h.persistAccount(accountCredential{
		UID: "cn-1", Domain: "www.codebuddy.cn", AccessToken: "at", RefreshToken: "rt", ExpiresIn: 60,
	}, "cn")
	if err != nil || status != http.StatusOK {
		t.Fatalf("persist status=%d err=%v", status, err)
	}
	if res["ok"] != true {
		t.Fatalf("response=%v", res)
	}
	if p.AuthByUID("cn-1") == nil {
		t.Fatal("new account must be added to the pool")
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["region"] != "all" {
		t.Fatalf("region=%v want all", doc["region"])
	}
}

// TestSMSLoginStatusMapping 错误到状态码的映射决定控制台是"提示重发"还是"直接报错"。
func TestSMSLoginStatusMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{smslogin.ErrSessionNotFound, http.StatusGone},
		{smslogin.ErrGlobalUnsupported, http.StatusBadRequest},
		{&smslogin.Error{Step: "校验验证码", Msg: "验证码不正确"}, http.StatusBadRequest},
		{&smslogin.Error{Step: "发送验证码", Msg: "OneID 拒绝"}, http.StatusBadRequest},
		{&smslogin.Error{Step: "写入凭证", Msg: "未生效"}, http.StatusBadGateway},
	}
	for _, c := range cases {
		if got := smsLoginStatus(c.err); got != c.want {
			t.Errorf("smsLoginStatus(%v)=%d want %d", c.err, got, c.want)
		}
	}
}
