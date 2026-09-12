package server

import (
	"context"
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

// TestAdminSMSSendExistingMobilePrompts 手机号已在号池中时必须先提示、且**不发短信**：
// 发一条短信有成本，用户通常没必要为一个已有账号重新登录。
func TestAdminSMSSendExistingMobilePrompts(t *testing.T) {
	h, p, _ := smsTestHandler(t, true)
	// 中国区账号的 nickname 就是号码本身（无区号）。
	p.Add(&auth.Auth{UID: "uid-existing", Nickname: "64087495", Domain: "www.codebuddy.cn"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/send",
		strings.NewReader(`{"mobile":"+852 64087495","region":"cn"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("existing mobile should prompt, code=%d body=%s", rec.Code, rec.Body)
	}
	var res struct {
		Existing bool   `json:"existing"`
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Existing || res.UID != "uid-existing" {
		t.Fatalf("response=%s", rec.Body)
	}
	// 关键：不能返回 session_id，说明确实没有走发码。
	if strings.Contains(rec.Body.String(), "session_id") {
		t.Fatalf("no sms must be sent before confirmation: %s", rec.Body)
	}
}

// TestAdminSMSSendForceBypassesPrompt 用户确认后带 force 再来，才真正发码。
func TestAdminSMSSendForceBypassesPrompt(t *testing.T) {
	h, p, _ := smsTestHandler(t, true)
	p.Add(&auth.Auth{UID: "uid-existing", Nickname: "64087495", Domain: "www.codebuddy.cn"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/send",
		strings.NewReader(`{"mobile":"+852 64087495","region":"cn","force":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("force should send, code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "session_id") {
		t.Fatalf("force must actually send the code: %s", rec.Body)
	}
}

// TestAdminSMSSendDifferentMobileNotBlocked 号池里有别的号码时不能误拦。
func TestAdminSMSSendDifferentMobileNotBlocked(t *testing.T) {
	h, p, _ := smsTestHandler(t, true)
	p.Add(&auth.Auth{UID: "uid-other", Nickname: "13800138000", Domain: "www.codebuddy.cn"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/send",
		strings.NewReader(`{"mobile":"+852 64087495","region":"cn"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("different number must not be blocked, code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestAdminSMSCaptchaHandedToUser 未配置打码密钥时，发码接口应回 200 并给出
// 可渲染的挑战，让用户自己在界面上过码——而不是把这条路堵死。
func TestAdminSMSCaptchaHandedToUser(t *testing.T) {
	h, _, authDir := smsTestHandler(t, true)
	h.cfg.SMSLogin = smslogin.NewManager(captchaUpstream(t).Endpoints(), 0)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/send",
		strings.NewReader(`{"mobile":"+852 64087495","region":"cn"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Status    string `json:"status"`
		SessionID string `json:"session_id"`
		Captcha   *struct {
			Options []struct {
				AppID     string `json:"appId"`
				CloudType string `json:"cloudType"`
			} `json:"options"`
			Reason        string `json:"reason"`
			AutoAttempted bool   `json:"auto_attempted"`
		} `json:"captcha"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "need_captcha" || resp.SessionID == "" {
		t.Fatalf("resp=%+v", resp)
	}
	if resp.Captcha == nil || len(resp.Captcha.Options) != 2 {
		t.Fatalf("captcha must be handed to the frontend: %s", rec.Body)
	}
	// 前端要靠 appId/cloudType 渲染正确的验证码，字段名必须是上游原名。
	if resp.Captcha.Options[0].CloudType != "teg" || resp.Captcha.Options[1].AppID != "197561220" {
		t.Fatalf("options=%+v", resp.Captcha.Options)
	}
	if resp.Captcha.AutoAttempted {
		t.Fatal("nothing was attempted without a solver")
	}
	if resp.Captcha.Reason == "" {
		t.Fatal("reason should explain why it was not solved automatically")
	}
	// 还没真正发码，不该落盘账号。
	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("no auth file should be written, got %d", len(entries))
	}
}

// TestAdminSMSSubmitCaptchaEndToEnd 手动过码后提交票据，应完成发码并可继续验码落盘。
func TestAdminSMSSubmitCaptchaEndToEnd(t *testing.T) {
	up := captchaUpstream(t)
	m := smslogin.NewManager(up.Endpoints(), 0)

	authDir := t.TempDir()
	p := testPoolWith()
	h := NewHandler(Config{Pool: p, AuthDir: authDir, Region: "cn", SMSLogin: m})

	// 先拿挑战。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/send",
		strings.NewReader(`{"mobile":"+852 64087495","region":"cn"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("send code=%d body=%s", rec.Code, rec.Body)
	}
	var send struct {
		SessionID string    `json:"session_id"`
		Captcha   *struct{} `json:"captcha"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &send)
	if send.Captcha == nil {
		t.Fatalf("expected a challenge: %s", rec.Body)
	}

	// 提交人工过码结果。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/captcha",
		strings.NewReader(`{"session_id":"`+send.SessionID+`","ticket":"t-manual","rand_str":"@r-manual","cloud_type":"tencent"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("submit captcha code=%d body=%s", rec.Code, rec.Body)
	}
	var solved struct {
		Status    string `json:"status"`
		ExpiresIn int64  `json:"expires_in"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &solved)
	if solved.Status != "unexpired" || solved.ExpiresIn == 0 {
		t.Fatalf("expected a live code, got %s", rec.Body)
	}
	// 上游确实收到了回灌的票据。
	if got := up.LastCaptchaVerification(); got == nil || got["ticket"] != "t-manual" {
		t.Fatalf("captchaVerification=%v", got)
	}

	// 过码后即可正常验码落盘。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/verify",
		strings.NewReader(`{"session_id":"`+send.SessionID+`","code":"123456"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("verify code=%d body=%s", rec.Code, rec.Body)
	}
	if p.AuthByUID("uid-sms-1") == nil {
		t.Fatal("account should be added")
	}
}

// TestAdminSMSVerifyBeforeCaptcha 还没过码就验码，要明确回报 captcha_pending，
// 让前端把用户导回验证码步骤，而不是显示一句看不懂的上游错误。
func TestAdminSMSVerifyBeforeCaptcha(t *testing.T) {
	up := captchaUpstream(t)
	h := NewHandler(Config{Pool: testPoolWith(), AuthDir: t.TempDir(), Region: "cn",
		SMSLogin: smslogin.NewManager(up.Endpoints(), 0)})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/send",
		strings.NewReader(`{"mobile":"+852 64087495","region":"cn"}`)))
	var send struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &send)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/verify",
		strings.NewReader(`{"session_id":"`+send.SessionID+`","code":"123456"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Reason    string `json:"reason"`
		Retryable bool   `json:"retryable"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Reason != "captcha_pending" || !body.Retryable {
		t.Fatalf("body=%s", rec.Body)
	}
}

// TestCaptchaUpstreamHelper 确认 helper 真的产出了挑战，避免测试自欺。
func TestCaptchaUpstreamHelper(t *testing.T) {
	up := captchaUpstream(t)
	if !up.needCaptcha {
		t.Fatal("captchaUpstream must set needCaptcha")
	}
}

// TestAdminSMSVerifyBeforeCaptcha 的辅助：确认挑战 helper 与假上游可用。

// TestAdminSMSCaptchaSolvedEndToEnd 配上 solver 后，need_captcha 应自动过码、
// 回灌票据并完成发码，用户全程无感。
func TestAdminSMSCaptchaSolvedEndToEnd(t *testing.T) {
	up := captchaUpstream(t)
	m := smslogin.NewManager(up.Endpoints(), 0)
	m.SetSolver(&stubSolver{ticket: "tr03_ticket", randStr: "@randstr"})

	authDir := t.TempDir()
	p := testPoolWith()
	h := NewHandler(Config{Pool: p, AuthDir: authDir, Region: "cn", SMSLogin: m})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/send",
		strings.NewReader(`{"mobile":"+852 64087495","region":"cn"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("solved captcha should send, code=%d body=%s", rec.Code, rec.Body)
	}
	var send struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &send)
	if send.SessionID == "" {
		t.Fatalf("no session: %s", rec.Body)
	}
	// 过码后链路必须继续可走完。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/verify",
		strings.NewReader(`{"session_id":"`+send.SessionID+`","code":"123456"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("verify after solved captcha: code=%d body=%s", rec.Code, rec.Body)
	}
	if p.AuthByUID("uid-sms-1") == nil {
		t.Fatal("account should be added after solving the captcha")
	}
}

// captchaUpstream 返回一个要求人机校验的假上游。
func captchaUpstream(t *testing.T) *fakeSMSUpstream {
	t.Helper()
	up := newFakeSMSUpstream(t)
	up.needCaptcha = true
	t.Cleanup(up.Close)
	return up
}

// stubSolver 是固定返回票据的假解算器。
type stubSolver struct {
	ticket  string
	randStr string
	err     error
}

func (s *stubSolver) Solve(context.Context, string) (string, string, error) {
	if s.err != nil {
		return "", "", s.err
	}
	return s.ticket, s.randStr, nil
}

// TestAdminSMSVerifyExistingReportsNotice 重新登录已有账号时，响应要能区分
// "新增"与"刷新凭证"，否则用户以为多了一个账号。
func TestAdminSMSVerifyExistingReportsNotice(t *testing.T) {
	h, p, _ := smsTestHandler(t, true)
	// 先正常添加一次。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/send",
		strings.NewReader(`{"mobile":"+852 64087495","region":"cn"}`)))
	var send struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &send)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/verify",
		strings.NewReader(`{"session_id":"`+send.SessionID+`","code":"123456"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("first add code=%d body=%s", rec.Code, rec.Body)
	}
	var first struct {
		Existed bool `json:"existed"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &first)
	if first.Existed {
		t.Fatalf("first add must not report existed: %s", rec.Body)
	}

	// 再登录一次同一号码（force 跳过提示）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/send",
		strings.NewReader(`{"mobile":"+852 64087495","region":"cn","force":true}`)))
	_ = json.Unmarshal(rec.Body.Bytes(), &send)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/verify",
		strings.NewReader(`{"session_id":"`+send.SessionID+`","code":"123456"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-add code=%d body=%s", rec.Code, rec.Body)
	}
	var second struct {
		Existed bool   `json:"existed"`
		Notice  string `json:"notice"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if !second.Existed {
		t.Fatalf("re-login should report existed: %s", rec.Body)
	}
	if second.Notice == "" {
		t.Fatalf("re-login should carry a notice: %s", rec.Body)
	}
	// 仍然只有一个账号，不能被重复添加。
	if n := len(p.List()); n != 1 {
		t.Fatalf("pool should still hold 1 account, got %d", n)
	}
}

// TestAdminSMSVerifyExistingDisabledWarns 账号此前被禁用时，重登不会自动启用，
// 必须明确提示，否则用户以为登录成功却始终用不上。
func TestAdminSMSVerifyExistingDisabledWarns(t *testing.T) {
	h, p, _ := smsTestHandler(t, true)
	p.Add(&auth.Auth{UID: "uid-sms-1", Nickname: "64087495", Domain: "www.codebuddy.cn"})
	p.Disable("uid-sms-1", "test")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/send",
		strings.NewReader(`{"mobile":"+852 64087495","region":"cn","force":true}`)))
	var send struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &send)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/verify",
		strings.NewReader(`{"session_id":"`+send.SessionID+`","code":"123456"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var res struct {
		Existed  bool   `json:"existed"`
		Disabled bool   `json:"disabled"`
		Notice   string `json:"notice"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Existed || !res.Disabled {
		t.Fatalf("must report existed+disabled: %s", rec.Body)
	}
	if !strings.Contains(res.Notice, "禁用") {
		t.Fatalf("notice should mention the disabled state: %q", res.Notice)
	}
	// 重登不应改变禁用状态。
	if st, ok := p.Status("uid-sms-1"); !ok || !st.Disabled {
		t.Fatal("re-login must not silently re-enable the account")
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
		{smslogin.ErrSessionBusy, http.StatusConflict},
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

// TestSMSLoginErrorBodyRetryable 前端靠 retryable/reason 决定是保留当前会话
// （直接重填验证码）还是清空要求重新发码，字段语义必须稳定。
func TestSMSLoginErrorBodyRetryable(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantRetry  bool
		wantReason string
	}{
		{
			name:       "code wrong keeps session",
			err:        &smslogin.Error{Step: "校验验证码", Msg: "验证码不正确，请重新填写", Err: smslogin.ErrCodeWrong, Retryable: true},
			wantRetry:  true,
			wantReason: "code_wrong",
		},
		{
			name:       "too frequent keeps session",
			err:        &smslogin.Error{Step: "发送验证码", Msg: "发送太频繁", Err: smslogin.ErrTooFrequent, Retryable: true},
			wantRetry:  true,
			wantReason: "too_frequent",
		},
		{
			name:       "expired session is not retryable",
			err:        smslogin.ErrSessionNotFound,
			wantRetry:  false,
			wantReason: "session_expired",
		},
		{
			name:       "busy is not retryable",
			err:        smslogin.ErrSessionBusy,
			wantRetry:  false,
			wantReason: "busy",
		},
		{
			name:       "generic upstream error is not retryable",
			err:        &smslogin.Error{Step: "写入凭证", Msg: "未生效"},
			wantRetry:  false,
			wantReason: "",
		},
		{
			name:       "ticket pending keeps session",
			err:        &smslogin.Error{Step: "取回凭证", Msg: "写票后 CLI 仍无凭证", Retryable: true},
			wantRetry:  true,
			wantReason: "ticket_pending",
		},
	}
	for _, c := range cases {
		body := smsLoginErrorBody(c.err)
		if body["retryable"] != c.wantRetry {
			t.Errorf("%s: retryable=%v want %v", c.name, body["retryable"], c.wantRetry)
		}
		if got := body["reason"]; got != c.wantReason && !(got == nil && c.wantReason == "") {
			t.Errorf("%s: reason=%v want %q", c.name, got, c.wantReason)
		}
		if body["error"] == "" {
			t.Errorf("%s: error message must not be empty", c.name)
		}
	}
}

// TestAdminSMSWrongCodeKeepsSession 输错验证码后必须能用同一 session_id 成功，
// 否则用户每输错一次就要多收一条短信。
func TestAdminSMSWrongCodeKeepsSession(t *testing.T) {
	h, _, authDir := smsTestHandler(t, true)

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

	// 先输错一次。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/verify",
		strings.NewReader(`{"session_id":"`+send.SessionID+`","code":"000000"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong code=%d body=%s", rec.Code, rec.Body)
	}
	var failBody struct {
		Retryable bool   `json:"retryable"`
		Reason    string `json:"reason"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &failBody)
	if !failBody.Retryable || failBody.Reason != "code_wrong" {
		t.Fatalf("wrong-code response must be retryable: %s", rec.Body)
	}

	// 同一个 session 再填对，必须成功落盘。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/account/sms/verify",
		strings.NewReader(`{"session_id":"`+send.SessionID+`","code":"123456"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("retry after wrong code failed: code=%d body=%s", rec.Code, rec.Body)
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one auth file, got %d", len(entries))
	}
}
