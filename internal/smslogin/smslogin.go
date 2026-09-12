// Package smslogin 实现 CodeBuddy 中国区的「短信直登」：服务端代替浏览器把
// OneID（腾讯统一账号）→ Keycloak → Console 的全套 HTTP 步骤走完，直接拿到账号凭证，
// 省掉人工开浏览器点授权。
//
// 流程（对齐 codebuddy-cli-api-login.md）：
//
//  1. POST {cli}/v2/plugin/auth/state         → CLI state + authUrl
//  2. POST {oneid}/v1/auth/sms/code/send      → state_token（真正发短信）
//  3. POST {oneid}/v1/auth/sms/code/verify    → 新 state_token
//  4. GET  {oneid}/v1/auth/accounts           → OneID authorization code
//  5. GET  {console}/auth/realms/{realm}/…    → broker loginUrl（每次现取，避开 session_code 短命）
//  6. GET  {loginUrl}&from_oneid_login=true   → Keycloak broker state + redirect_uri
//  7. GET  {redirect_uri}?code&state          → 落 KEYCLOAK_IDENTITY
//  8. GET  {console}/console/accounts         → 触发 APISIX OIDC，落 session
//  9. POST {console}/console/login/enterprise → Console accessToken
//  10. GET  {console}/console/auth/login       → 把一次性票写给该 state（看 Location: /）
//  11. GET  {cli}/v2/plugin/auth/token         → 账号 accessToken / refreshToken / domain
//  12. GET  {cli}/v2/plugin/login/account      → uid / nickname / enterpriseId
//
// 第 10 步那张票是"谁先 GET 谁拿走"的一次性凭据。本场景没有 CLI 在等待，
// 由我们自己消费，因此不存在与 CLI 抢票的问题；反过来，正式接 CLI 时不要复用本包。
//
// 安全：state_token、Cookie、JWT、短信验证码一律不写日志。OneID 的错误消息会把
// 请求里带的 token 原样回显（实测 "无效的tokenxxx"），因此所有上游错误在向上
// 传播前都过一遍 redact。
package smslogin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	cliUA = "CLI/2.63.2 CodeBuddy/2.63.2"

	// consoleDomain 是 X-Domain 头与 Keycloak redirect_uri 使用的站点。
	consoleDomain = "www.codebuddy.cn"

	maxBodyBytes = 1 << 20
	maxRedirects = 10
)

// ErrSessionNotFound 表示会话不存在或已过期，调用方应提示用户重新发码。
var ErrSessionNotFound = errors.New("短信登录会话不存在或已过期，请重新发送验证码")

// ErrGlobalUnsupported 表示请求了非中国区。短信流程依赖 codebuddy.cn 的
// OneID/Keycloak 布局，海外版走不通。
var ErrGlobalUnsupported = errors.New("短信登录仅支持中国区，海外版请使用 OAuth 链接登录")

// Endpoints 是流程涉及的三台上游主机。抽出来是为了让测试能把整套流程
// 指向 httptest 服务器。
type Endpoints struct {
	Console string // https://www.codebuddy.cn
	CLI     string // https://copilot.tencent.com
	OneID   string // https://oauth2.account.tencent.com
	Realm   string // Keycloak realm，中国区为 copilot
}

// DefaultEndpoints 返回生产环境地址。
func DefaultEndpoints() Endpoints {
	return Endpoints{
		Console: "https://www.codebuddy.cn",
		CLI:     "https://copilot.tencent.com",
		OneID:   "https://oauth2.account.tencent.com",
		Realm:   "copilot",
	}
}

func (e Endpoints) authEndpoint() string {
	return e.Console + "/auth/realms/" + e.Realm + "/protocol/openid-connect/auth"
}

// SendResult 是"已发出验证码"的回执。
type SendResult struct {
	SessionID string `json:"session_id"`
	Mobile    string `json:"mobile"`
	Status    string `json:"status,omitempty"`
	ExpiresIn int64  `json:"expires_in,omitempty"`
}

// Credentials 是一次成功短信登录换到的账号凭证，形状与 OAuth 登录一致。
type Credentials struct {
	UID          string
	Nickname     string
	EnterpriseID string
	Domain       string
	Region       string
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
}

// Error 给上游失败标注发生在那一步，便于控制台直接展示。
type Error struct {
	Step string
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Step + "：" + e.Msg + "（" + e.Err.Error() + "）"
	}
	return e.Step + "：" + e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

func stepErr(step, msg string, err error) error { return &Error{Step: step, Msg: msg, Err: err} }

// Manager 持有进行中的短信登录会话。会话里保存了本次流程的 CLI state 与
// 两套独立 Cookie（OneID 与 CodeBuddy），换号时天然隔离，不会复用上一个号的
// Keycloak/APISIX 身份。
type Manager struct {
	ep  Endpoints
	ttl time.Duration

	mu       sync.Mutex
	sessions map[string]*session

	// 测试注入点。
	now     func() time.Time
	randHex func(n int) (string, error)
}

// NewManager 构建 Manager。ttl 为单次短信登录会话的有效期，0 表示默认 10 分钟。
func NewManager(ep Endpoints, ttl time.Duration) *Manager {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &Manager{
		ep:       ep,
		ttl:      ttl,
		sessions: make(map[string]*session),
		now:      time.Now,
		randHex:  randomHex,
	}
}

// session 是单次短信登录的全部中间状态。
type session struct {
	id       string
	mobile   string
	cliState string
	authURL  string
	oneIDTok string
	created  time.Time

	consoleHTTP   *http.Client // 跟随跳转（broker 链、APISIX OIDC）
	consoleNoJump *http.Client // 不跟随跳转（读 Location 判断写票结果）
	oneIDHTTP     *http.Client
	cliHTTP       *http.Client
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Send 生成 CLI state 并触发 OneID 下发短信验证码。
//
// 之所以在发码前就取 CLI state：如果上游不可达，用户在"发送验证码"这一步就会
// 看到失败，而不是白白消耗一条短信。
func (m *Manager) Send(ctx context.Context, mobile, region string) (SendResult, error) {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		region = "cn"
	}
	if region != "cn" && region != "china" {
		return SendResult{}, ErrGlobalUnsupported
	}
	normalized, err := NormalizeMobile(mobile)
	if err != nil {
		return SendResult{}, err
	}

	sess, err := m.newSession(normalized)
	if err != nil {
		return SendResult{}, err
	}

	// 1) CLI state。
	if err := sess.fetchCLIState(ctx, m.ep); err != nil {
		return SendResult{}, err
	}

	// 2) 发短信。
	status, expiresIn, token, err := sess.oneIDSend(ctx, m.ep)
	if err != nil {
		return SendResult{}, err
	}
	if token == "" {
		return SendResult{}, stepErr("发送验证码", "上游未返回 state_token", nil)
	}
	sess.oneIDTok = token

	m.store(sess)
	return SendResult{
		SessionID: sess.id,
		Mobile:    sess.mobile,
		Status:    status,
		ExpiresIn: expiresIn,
	}, nil
}

// Verify 校验短信验证码并走完剩余全部步骤，返回账号凭证。
// 成功后会话即被销毁：OneID code、Keycloak 会话、一次性票都是一次性的。
func (m *Manager) Verify(ctx context.Context, sessionID, code string) (Credentials, error) {
	sessionID = strings.TrimSpace(sessionID)
	code = strings.TrimSpace(code)
	if sessionID == "" {
		return Credentials{}, ErrSessionNotFound
	}
	if code == "" {
		return Credentials{}, errors.New("验证码不能为空")
	}

	sess, err := m.take(sessionID)
	if err != nil {
		return Credentials{}, err
	}
	// 会话一旦取出就不再放回：无论成败，其中的中间凭据都已作废。
	defer sess.close()

	// 3) 验码。
	if err := sess.oneIDVerify(ctx, m.ep, code); err != nil {
		return Credentials{}, err
	}

	// 4) 拉 OneID 账号，拿 broker 用的 authorization code。
	oneIDCode, err := sess.oneIDAccounts(ctx, m.ep)
	if err != nil {
		return Credentials{}, err
	}

	// 5) 现取 broker loginUrl：Keycloak session_code 短命，用发码时那份可能已过期。
	//    带上同一个 AUTH_SESSION_ID Cookie 重取，会复用同一认证会话并换发新 session_code。
	loginURL, err := sess.brokerLoginURL(ctx, m.ep)
	if err != nil {
		return Credentials{}, err
	}

	// 6) 把 OneID 结果交给 Keycloak。
	brokerState, redirectURI, err := sess.brokerStart(ctx, loginURL)
	if err != nil {
		return Credentials{}, err
	}

	// 7) 走完 broker 重定向链，落 KEYCLOAK_IDENTITY。
	if err := sess.brokerFinish(ctx, redirectURI, oneIDCode, brokerState); err != nil {
		return Credentials{}, err
	}

	// 8) 让 APISIX 用刚拿到的 Keycloak SSO 建 /console 会话。
	if err := sess.establishConsoleSession(ctx, m.ep); err != nil {
		return Credentials{}, err
	}

	// 9) 选账号（个人号 enterpriseId 为空）。
	consoleToken, err := sess.enterpriseLogin(ctx, m.ep)
	if err != nil {
		return Credentials{}, err
	}

	// 10) 把一次性票写给该 state，随后由我们自己取走（没有 CLI 在等）。
	if err := sess.writeTicket(ctx, m.ep, consoleToken); err != nil {
		return Credentials{}, err
	}

	// 11) 取票：这条路径与 cmd/login poll 完全一致，保证落盘凭证形状不变。
	ticket, err := sess.fetchTicket(ctx, m.ep)
	if err != nil {
		return Credentials{}, err
	}

	// 12) 账号信息（uid / nickname / enterpriseId）。
	account, err := sess.lookupAccount(ctx, m.ep, ticket.AccessToken)
	if err != nil {
		return Credentials{}, err
	}
	if account.UID == "" {
		return Credentials{}, stepErr("读取账号", "上游未返回账号 UID", nil)
	}

	domain := strings.TrimSpace(ticket.Domain)
	if domain == "" {
		domain = consoleDomain
	}
	return Credentials{
		UID:          account.UID,
		Nickname:     account.Nickname,
		EnterpriseID: account.EnterpriseID,
		Domain:       domain,
		Region:       "cn",
		AccessToken:  ticket.AccessToken,
		RefreshToken: ticket.RefreshToken,
		ExpiresIn:    ticket.ExpiresIn,
	}, nil
}

// closed 之后 session 的中间凭据全部丢弃。
func (s *session) close() {
	s.oneIDTok = ""
	s.cliState = ""
}

func (m *Manager) newSession(mobile string) (*session, error) {
	id, err := m.randHex(16)
	if err != nil {
		return nil, err
	}
	consoleJar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	oneIDJar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &session{
		id:      id,
		mobile:  mobile,
		created: m.now(),
		oneIDHTTP: &http.Client{
			Timeout: 30 * time.Second, Jar: oneIDJar,
			CheckRedirect: noRedirectLimit,
		},
		consoleHTTP: &http.Client{
			Timeout: 30 * time.Second, Jar: consoleJar,
			CheckRedirect: noRedirectLimit,
		},
		consoleNoJump: &http.Client{
			Timeout: 30 * time.Second, Jar: consoleJar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		cliHTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirectLimit},
	}, nil
}

func noRedirectLimit(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("重定向次数过多")
	}
	return nil
}

func (m *Manager) store(s *session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked()
	m.sessions[s.id] = s
}

// take 取出并删除会话，顺带清理过期项。
func (m *Manager) take(id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrSessionNotFound
	}
	delete(m.sessions, id)
	return s, nil
}

// gcLocked 清理超时会话。调用方需持有 m.mu。
func (m *Manager) gcLocked() {
	deadline := m.now().Add(-m.ttl)
	for id, s := range m.sessions {
		if s.created.Before(deadline) {
			delete(m.sessions, id)
		}
	}
}

// ---------------------------------------------------------------------------
// 各步骤实现
// ---------------------------------------------------------------------------

// fetchCLIState 第 1 步：向 CLI 网关申请本次登录的 state 与授权链接。
func (s *session) fetchCLIState(ctx context.Context, ep Endpoints) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ep.CLI+"/v2/plugin/auth/state?platform=CLI", bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	commonJSON(req)
	req.Header.Set("User-Agent", cliUA)

	var data struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := s.doEnvelope(s.cliHTTP, req, &data); err != nil {
		return stepErr("初始化登录", "无法获取 CLI 登录状态", err)
	}
	if data.State == "" {
		return stepErr("初始化登录", "上游未返回 state", nil)
	}
	s.cliState = data.State
	s.authURL = data.AuthURL
	return nil
}

// oneIDSend 第 2 步：下发短信验证码。
func (s *session) oneIDSend(ctx context.Context, ep Endpoints) (status string, expiresIn int64, token string, err error) {
	payload := map[string]any{
		"client_code": "codebuddy",
		"mobile":      s.mobile,
		"scopes":      []string{"openid", "mobile", "profile"},
	}
	raw, _ := json.Marshal(payload)
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, ep.OneID+"/v1/auth/sms/code/send", bytes.NewReader(raw))
	if reqErr != nil {
		return "", 0, "", reqErr
	}
	commonJSON(req)

	var resp oneIDSendResponse
	if err = s.doOneID(s.oneIDHTTP, req, &resp); err != nil {
		return "", 0, "", stepErr("发送验证码", "OneID 拒绝了本次请求", s.redactErr(err))
	}
	status = firstNonEmpty(resp.Status, resp.Data.Status)
	if status == "need_captcha" || len(resp.Captcha) > 0 || len(resp.Data.Captcha) > 0 {
		return "", 0, "", stepErr("发送验证码", "该号码需要图形验证码，请稍后重试或改用浏览器授权登录", nil)
	}
	return status, firstNonZero(resp.ExpiresIn, resp.Data.ExpiresIn), firstNonEmpty(resp.StateToken, resp.Data.StateToken), nil
}

// oneIDVerify 第 3 步：校验短信验证码，换取新的 state_token。
func (s *session) oneIDVerify(ctx context.Context, ep Endpoints, code string) error {
	payload := map[string]any{"state_token": s.oneIDTok, "code": code}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.OneID+"/v1/auth/sms/code/verify", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	commonJSON(req)

	var resp oneIDSendResponse
	if err := s.doOneID(s.oneIDHTTP, req, &resp); err != nil {
		return stepErr("校验验证码", "验证码不正确或已过期", s.redactErr(err))
	}
	if tok := firstNonEmpty(resp.StateToken, resp.Data.StateToken); tok != "" {
		s.oneIDTok = tok
	}
	return nil
}

// oneIDAccounts 第 4 步：拉取 OneID 账号列表，取顶层 authorization code。
func (s *session) oneIDAccounts(ctx context.Context, ep Endpoints) (string, error) {
	u := ep.OneID + "/v1/auth/accounts?state_token=" + url.QueryEscape(s.oneIDTok)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	commonJSON(req)

	var resp struct {
		Accounts []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"accounts"`
		Code     string `json:"code"`
		NextStep string `json:"next_step"`
		Data     struct {
			Accounts []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"accounts"`
			Code     string `json:"code"`
			NextStep string `json:"next_step"`
		} `json:"data"`
	}
	if err := s.doOneID(s.oneIDHTTP, req, &resp); err != nil {
		return "", stepErr("读取账号", "无法获取 OneID 账号", s.redactErr(err))
	}
	accounts := resp.Accounts
	if len(accounts) == 0 {
		accounts = resp.Data.Accounts
	}
	if len(accounts) > 1 {
		// 多账号需要额外的 select_account 选择步骤；在没有可靠接口形状前
		// 不做猜测，直接让用户走浏览器授权，避免选错账号。
		names := make([]string, 0, len(accounts))
		for _, a := range accounts {
			names = append(names, firstNonEmpty(a.Name, a.ID))
		}
		return "", stepErr("读取账号", "该手机号下有多个账号（"+strings.Join(names, "、")+"），请改用浏览器授权登录并手动选择", nil)
	}
	code := firstNonEmpty(resp.Code, resp.Data.Code)
	if code == "" {
		return "", stepErr("读取账号", "OneID 未返回授权码", nil)
	}
	return code, nil
}

var brokerHrefRe = regexp.MustCompile(`href="([^"]*broker/oneid/login[^"]*)"`)

// brokerLoginURL 第 5 步：打开 Keycloak 登录页，从 HTML 里取出 OneID broker 地址。
// 该地址带 session_code 与 tab_id，绑定本次 Keycloak 认证会话，因此每次现取。
func (s *session) brokerLoginURL(ctx context.Context, ep Endpoints) (string, error) {
	redirect := ep.Console + "/login/?platform=CLI&state=" + url.QueryEscape(s.cliState)
	q := url.Values{}
	q.Set("client_id", "console")
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirect)
	q.Set("skip_redirect_confirm", "true")
	q.Set("platform", "CLI")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.authEndpoint()+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	browserHeaders(req)
	resp, err := s.consoleNoJump.Do(req)
	if err != nil {
		return "", stepErr("打开登录页", "无法访问 Keycloak", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))

	if resp.StatusCode/100 == 3 {
		// 已有有效 KEYCLOAK_IDENTITY 会直接 SSO 回旧账号。本流程每个会话都用
		// 全新 Cookie jar，正常不会走到这里；真走到说明 Cookie 被复用，必须
		// 失败而不是静默登成别人。
		return "", stepErr("打开登录页", "检测到复用的 Keycloak 会话，已中止以避免登录到其他账号", nil)
	}
	if resp.StatusCode != http.StatusOK {
		return "", stepErr("打开登录页", fmt.Sprintf("Keycloak 返回 HTTP %d", resp.StatusCode), nil)
	}
	doc := strings.ReplaceAll(string(body), "&amp;", "&")
	match := brokerHrefRe.FindStringSubmatch(doc)
	if match == nil {
		return "", stepErr("打开登录页", "登录页中未找到 OneID 入口", nil)
	}
	loginURL := match[1]
	if strings.HasPrefix(loginURL, "/") {
		loginURL = ep.Console + loginURL
	}
	return loginURL, nil
}

// brokerStart 第 6 步：告诉 Keycloak 走 OneID 登录，取回 broker state 与 redirect_uri。
func (s *session) brokerStart(ctx context.Context, loginURL string) (state, redirectURI string, err error) {
	sep := "?"
	if strings.Contains(loginURL, "?") {
		sep = "&"
	}
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, loginURL+sep+"from_oneid_login=true", nil)
	if reqErr != nil {
		return "", "", reqErr
	}
	browserHeaders(req)

	var data struct {
		State       string `json:"state"`
		RedirectURI string `json:"redirect_uri"`
	}
	if err = s.doEnvelope(s.consoleHTTP, req, &data); err != nil {
		return "", "", stepErr("交接登录", "Keycloak 未接受 OneID 登录", err)
	}
	if data.State == "" || data.RedirectURI == "" {
		return "", "", stepErr("交接登录", "Keycloak 未返回 broker state 或回调地址", nil)
	}
	return data.State, data.RedirectURI, nil
}

// brokerFinish 第 7 步：走完 broker 重定向链，落 KEYCLOAK_IDENTITY。
func (s *session) brokerFinish(ctx context.Context, redirectURI, code, state string) error {
	q := url.Values{}
	q.Set("code", code)
	q.Set("state", state)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, redirectURI+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	browserHeaders(req)
	resp, err := s.consoleHTTP.Do(req)
	if err != nil {
		return stepErr("完成登录", "Keycloak 回调失败", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode >= 400 {
		return stepErr("完成登录", fmt.Sprintf("Keycloak 回调返回 HTTP %d", resp.StatusCode), nil)
	}
	return nil
}

// establishConsoleSession 第 8 步：访问 /console/accounts 触发 APISIX OIDC，
// 由网关用刚拿到的 Keycloak SSO 建立 /console 的 session Cookie。
func (s *session) establishConsoleSession(ctx context.Context, ep Endpoints) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.Console+"/console/accounts", nil)
	if err != nil {
		return err
	}
	browserHeaders(req)
	req.Header.Set("X-Domain", consoleDomain)

	resp, err := s.consoleHTTP.Do(req)
	if err != nil {
		return stepErr("建立控制台会话", "无法访问 Console", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode >= 400 {
		return stepErr("建立控制台会话", fmt.Sprintf("Console 返回 HTTP %d", resp.StatusCode), nil)
	}
	return nil
}

// enterpriseLogin 第 9 步：选定账号，拿 Console accessToken。
// 个人号 enterpriseId 为空，走不带路径参数的那条路由。
func (s *session) enterpriseLogin(ctx context.Context, ep Endpoints) (string, error) {
	u := ep.Console + "/console/login/enterprise?state=" + url.QueryEscape(s.cliState)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	commonJSON(req)
	req.Header.Set("X-Domain", consoleDomain)
	req.Header.Set("X-Product-Code", "CLI")

	var data struct {
		AccessToken string `json:"accessToken"`
	}
	if err := s.doEnvelope(s.consoleHTTP, req, &data); err != nil {
		return "", stepErr("选择账号", "Console 未返回访问令牌", err)
	}
	if data.AccessToken == "" {
		return "", stepErr("选择账号", "Console 未返回访问令牌", nil)
	}
	return data.AccessToken, nil
}

// writeTicket 第 10 步：把一次性票写给该 state。
//
// 判定口径按文档：302 到站点根即成功（前端 axios 会把 / 当成失败，CLI 只认取票接口）；
// 302 回 /login 表示没写上（通常因为漏了 Authorization）；200 + 业务 code 0 也算成功。
func (s *session) writeTicket(ctx context.Context, ep Endpoints, consoleToken string) error {
	q := url.Values{}
	q.Set("platform", "CLI")
	q.Set("state", s.cliState)
	q.Set("domain", consoleDomain)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.Console+"/console/auth/login?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	browserHeaders(req)
	req.Header.Set("X-Domain", consoleDomain)
	req.Header.Set("Authorization", "Bearer "+consoleToken)

	resp, err := s.consoleNoJump.Do(req)
	if err != nil {
		return stepErr("写入凭证", "写票请求失败", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))

	switch {
	case resp.StatusCode/100 == 3:
		if isRootLocation(resp.Header.Get("Location"), ep.Console) {
			return nil
		}
		return stepErr("写入凭证", "写票未生效，服务端要求重新登录", nil)
	case resp.StatusCode == http.StatusOK:
		var env struct {
			Code int `json:"code"`
		}
		if json.Unmarshal(body, &env) == nil && env.Code == 0 {
			return nil
		}
		return stepErr("写入凭证", "写票响应异常", nil)
	default:
		return stepErr("写入凭证", fmt.Sprintf("写票返回 HTTP %d", resp.StatusCode), nil)
	}
}

func isRootLocation(loc, console string) bool {
	loc = strings.TrimSpace(loc)
	if loc == "" {
		return false
	}
	if strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://") {
		u, err := url.Parse(loc)
		if err != nil {
			return false
		}
		return strings.TrimSuffix(u.Path, "/") == ""
	}
	return strings.TrimSuffix(loc, "/") == ""
}

type ticket struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// fetchTicket 第 11 步：取走写在该 state 上的一次性票。
func (s *session) fetchTicket(ctx context.Context, ep Endpoints) (ticket, error) {
	u := ep.CLI + "/v2/plugin/auth/token?state=" + url.QueryEscape(s.cliState)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ticket{}, err
	}
	commonJSON(req)
	req.Header.Set("User-Agent", cliUA)
	req.Header.Set("X-No-Authorization", "true")

	var t ticket
	if err := s.doEnvelope(s.cliHTTP, req, &t); err != nil {
		return ticket{}, stepErr("取回凭证", "上游未返回账号令牌", err)
	}
	if t.AccessToken == "" {
		return ticket{}, stepErr("取回凭证", "上游未返回账号令牌", nil)
	}
	return t, nil
}

// lookupAccount 第 12 步：用账号令牌换取 uid / nickname / enterpriseId。
// 字段解析与 cmd/login 保持一致，容忍嵌套形与扁平形两种响应。
func (s *session) lookupAccount(ctx context.Context, ep Endpoints, accessToken string) (Credentials, error) {
	u := ep.CLI + "/v2/plugin/login/account?state=" + url.QueryEscape(s.cliState)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Credentials{}, err
	}
	commonJSON(req)
	req.Header.Set("User-Agent", cliUA)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	var account struct {
		UID          string `json:"uid"`
		UserID       string `json:"userId"`
		ID           string `json:"id"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
		Name         string `json:"name"`
		Account      struct {
			UID          string `json:"uid"`
			UserID       string `json:"userId"`
			ID           string `json:"id"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			Name         string `json:"name"`
		} `json:"account"`
	}
	if err := s.doEnvelope(s.cliHTTP, req, &account); err != nil {
		return Credentials{}, stepErr("读取账号", "无法获取账号信息", err)
	}
	return Credentials{
		UID:          firstNonEmpty(account.UID, account.UserID, account.ID, account.Account.UID, account.Account.UserID, account.Account.ID),
		EnterpriseID: firstNonEmpty(account.EnterpriseID, account.Account.EnterpriseID),
		Nickname:     firstNonEmpty(account.Nickname, account.Name, account.Account.Nickname, account.Account.Name),
	}, nil
}

// ---------------------------------------------------------------------------
// HTTP 与解析辅助
// ---------------------------------------------------------------------------

func commonJSON(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	browserHeaders(req)
}

// browserHeaders 补齐浏览器形态的请求头。上游按 Referer/Origin 做来源校验。
func browserHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Origin", "https://"+consoleDomain)
	req.Header.Set("Referer", "https://"+consoleDomain+"/")
}

// doEnvelope 执行请求并解析 {code,msg,data} 信封。code != 0 视为失败。
func (s *session) doEnvelope(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("HTTP %d: 响应不是合法 JSON", resp.StatusCode)
	}
	if env.Code != 0 {
		return fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("解析 data 失败: %w", err)
		}
	}
	return nil
}

// doOneID 执行请求并解析 OneID 响应。OneID 不套 {code,data} 信封，
// 失败时返回 HTTP 4xx + {errCode,errMessage}。
func (s *session) doOneID(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	var envelope struct {
		ErrCode    string `json:"errCode"`
		ErrMessage string `json:"errMessage"`
	}
	_ = json.Unmarshal(body, &envelope)
	if envelope.ErrCode != "" {
		return fmt.Errorf("%s %s", envelope.ErrCode, envelope.ErrMessage)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("响应不是合法 JSON: %w", err)
	}
	return nil
}

// oneIDSendResponse 同时兼容顶层与嵌套在 data 下的两种字段位置。
type oneIDSendResponse struct {
	Status     string          `json:"status"`
	StateToken string          `json:"state_token"`
	ExpiresIn  int64           `json:"expires_in"`
	Captcha    json.RawMessage `json:"captcha"`
	Data       struct {
		Status     string          `json:"status"`
		StateToken string          `json:"state_token"`
		ExpiresIn  int64           `json:"expires_in"`
		Captcha    json.RawMessage `json:"captcha"`
	} `json:"data"`
}

// redacted 是 errMessage 里可能被回显的敏感串占位。
const redacted = "***"

// redactErr 抹掉错误消息里可能回显的敏感值。OneID 会把请求里的 state_token
// 原样拼进错误消息（实测 "无效的token<value>"），直接透出等于泄漏凭据。
//
// 先按本会话已知的凭据精确替换（短 token 也能覆盖），再用长度启发式兜底，
// 捕获消息里出现的其它未知长 token。
func (s *session) redactErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, secret := range []string{s.oneIDTok, s.cliState} {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, redacted)
		}
	}
	return errors.New(redact(msg))
}

var tokenLikeRe = regexp.MustCompile(`[A-Za-z0-9_\-\.]{24,}`)

// redact 替换掉长 token 形状的片段，保留错误码等可诊断信息。
func redact(msg string) string {
	return tokenLikeRe.ReplaceAllString(msg, redacted)
}

// NormalizeMobile 把用户输入的手机号整理成 OneID 要求的 "+区号 号码" 形式。
//
// 文档明确记录：香港号必须写成 "+852 64087495"（区号与号码之间有空格），
// 连写可能发码失败或触发图形验证码。因此这里统一补空格。
func NormalizeMobile(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("手机号不能为空")
	}
	// 用户已经手写了 "+区号 空格 号码" 时尊重其切分，避免自动猜错区号长度。
	if explicitMobileRe.MatchString(s) {
		parts := strings.Fields(s)
		digits := strings.TrimPrefix(parts[0], "+") + parts[1]
		if err := validateMobileDigits(digits); err != nil {
			return "", err
		}
		return parts[0] + " " + parts[1], nil
	}

	digits := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", "+", "").Replace(s)
	digits = strings.TrimPrefix(digits, "00")
	if err := validateMobileDigits(digits); err != nil {
		return "", err
	}
	cc := "86"
	switch {
	case strings.HasPrefix(digits, "852"), strings.HasPrefix(digits, "853"), strings.HasPrefix(digits, "886"):
		cc = digits[:3]
	}
	return "+" + cc + " " + strings.TrimPrefix(digits, cc), nil
}

var explicitMobileRe = regexp.MustCompile(`^\+\d{1,3}\s+\d{5,}$`)

func validateMobileDigits(digits string) error {
	if digits == "" {
		return errors.New("手机号不能为空")
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return errors.New("手机号只能包含数字")
		}
	}
	if len(digits) < 6 || len(digits) > 15 {
		return errors.New("手机号长度不合法")
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstNonZero(values ...int64) int64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}
