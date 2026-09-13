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
//  10. GET  {console}/console/auth/login       → 把一次性票写给该 state（不带 Bearer，看 Location: /login?force...）
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

// ErrSessionBusy 表示同一会话已有一次验码在进行中。上游对并发/高频操作返回
// E0010022，这里提前拦下，避免把上游频控额度浪费掉。
var ErrSessionBusy = errors.New("该会话正在验证中，请稍候再试")

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
//
// Captcha 非 nil 时表示这次发码被人机校验拦下：SessionID 有效，但还没有
// state_token，必须先在界面上完成验证码再提交，才会真的发出短信。
type SendResult struct {
	SessionID string         `json:"session_id"`
	Mobile    string         `json:"mobile"`
	Status    string         `json:"status,omitempty"`
	ExpiresIn int64          `json:"expires_in,omitempty"`
	Region    string         `json:"region,omitempty"`
	Captcha   *CaptchaPrompt `json:"captcha,omitempty"`
}

// CaptchaPrompt 告诉调用方需要人机校验，并给出可以交给前端渲染的方案。
//
// 之所以把上游的完整列表交给前端而不是替它挑一条：自动打码只认 tencent，
// 而人工打码两种都能过，让用户在界面上依次尝试的通过率更高。
type CaptchaPrompt struct {
	// Options 是上游给出的校验方案，顺序即尝试顺序。
	Options []CaptchaOption `json:"options"`
	// Reason 解释为什么没有自动过码（未配置密钥 / 平台失败）。
	Reason string `json:"reason,omitempty"`
	// AutoAttempted 表示已经尝试过自动打码但没成功。
	AutoAttempted bool `json:"auto_attempted,omitempty"`
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
	// Retryable 表示本次失败没有破坏登录会话：用户可以直接重填验证码，
	// 或点"重新发送"。不可重试（如 token 已失效）时调用方应丢弃会话。
	Retryable bool
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Step + "：" + e.Msg + "（" + e.Err.Error() + "）"
	}
	return e.Step + "：" + e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

// ErrCodeWrong 验证码填错。上游不会因此作废 state_token，可原地重填。
var ErrCodeWrong = errors.New("验证码错误")

// ErrTooFrequent 触发上游频控（E0010022）。稍后重试即可，会话不受影响。
var ErrTooFrequent = errors.New("操作太频繁")

// ErrCaptchaPending 表示会话还没通过人机校验，此时还没有 state_token，
// 不能直接验码。前端据此把用户导回验证码步骤。
var ErrCaptchaPending = errors.New("请先完成人机校验")

func stepErr(step, msg string, err error) error { return &Error{Step: step, Msg: msg, Err: err} }

// stepErrRetryable 标记"会话仍然可用"的失败。
func stepErrRetryable(step, msg string, err error) error {
	return &Error{Step: step, Msg: msg, Err: err, Retryable: true}
}

// Manager 持有进行中的短信登录会话。会话里保存了本次流程的 CLI state 与
// 两套独立 Cookie（OneID 与 CodeBuddy），换号时天然隔离，不会复用上一个号的
// Keycloak/APISIX 身份。
type Manager struct {
	ep  Endpoints
	ttl time.Duration
	// solver 用于自动过腾讯人机校验。nil = 未配置打码平台，
	// 此时遇到 need_captcha 仍然提示用户改用浏览器授权。
	solver Solver
	// proxy 为登录链路提供出口代理（规避同 IP 注册频控）。nil = 直连。
	proxy ProxyDialer
	// proxyDialTimeout 代理拨号超时。
	proxyDialTimeout time.Duration

	mu       sync.Mutex
	sessions map[string]*session

	// 测试注入点。
	now     func() time.Time
	randHex func(n int) (string, error)
	sleep   func(context.Context, time.Duration) error
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
		sleep:    sleepCtx,
	}
}

// SetSolver 装上人机校验解算器（通常是 2captcha）。
// 传入 nil 等价于关闭自动打码，行为回到"提示改用浏览器授权"。
func (m *Manager) SetSolver(s Solver) { m.solver = s }

// session 是单次短信登录的全部中间状态。
type session struct {
	id       string
	mobile   string
	cliState string
	authURL  string
	oneIDTok string
	created  time.Time
	// verified 表示短信已经验过。后续 11217 可以空码重跑写票/取票。
	verified bool
	// busy 表示正有一次 Verify 在跑。同一会话并发验码会被上游直接 406，
	// 因此这里做单飞保护。
	busy bool

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
	status, expiresIn, token, err := sess.oneIDSend(ctx, m.ep, nil)
	if err != nil {
		var need *captchaRequired
		if !errors.As(err, &need) {
			return SendResult{}, err
		}
		// 上游要求人机校验。先试自动打码；不行就把挑战交给用户在界面上完成。
		// 两条路都走不通才报错——手动作答是始终可用的兜底，尤其是 teg
		// 这种 2captcha 不支持的类型，只能靠人工过。
		verification, reason := m.tryAutoSolve(ctx, need.options)
		if verification == nil {
			m.store(sess)
			return SendResult{
				SessionID: sess.id,
				Mobile:    sess.mobile,
				Status:    "need_captcha",
				Region:    "cn",
				Captcha: &CaptchaPrompt{
					Options:       need.options,
					Reason:        reason,
					AutoAttempted: m.solver != nil,
				},
			}, nil
		}
		// 票据很短命，拿到就立刻回灌，不要做别的事。
		status, expiresIn, token, err = sess.oneIDSend(ctx, m.ep, verification)
		if err != nil {
			return SendResult{}, err
		}
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
		Region:    "cn",
	}, nil
}

// SubmitCaptcha 回灌用户在界面上完成的验证码结果，继续发码。
//
// 与自动过码走同一条回灌路径：上游只认 captchaVerification 这个对象，
// 不区分票据是打码平台给的还是真人在浏览器里点出来的。
func (m *Manager) SubmitCaptcha(ctx context.Context, sessionID string, ticket, randStr, cloudType string) (SendResult, error) {
	ticket = strings.TrimSpace(ticket)
	randStr = strings.TrimSpace(randStr)
	cloudType = strings.TrimSpace(cloudType)
	if ticket == "" || randStr == "" {
		return SendResult{}, stepErr("人机校验", "验证码结果不完整，请重新完成校验", ErrCaptchaIncomplete)
	}

	sess, err := m.claim(sessionID)
	if err != nil {
		return SendResult{}, err
	}
	// 会话一旦拿到 state_token 就不能再用这个入口，否则会把已发出的短信作废。
	keep := true
	defer func() { m.release(sessionID, keep) }()

	if cloudType == "" {
		cloudType = TwoCaptchaCloudType
	}
	// 文档：回灌字段是对象 {ticket, randStr, cloudType}，randStr 的 S 大写。
	// cloudType 必须与产出该票据的方案一致，否则上游直接判失败。
	status, expiresIn, token, err := sess.oneIDSend(ctx, m.ep, map[string]any{
		"ticket":    ticket,
		"randStr":   randStr,
		"cloudType": cloudType,
	})
	if err != nil {
		// 票据过期是最常见的失败：让它保留会话，用户可以重新取挑战再试一次。
		var need *captchaRequired
		if errors.As(err, &need) {
			return SendResult{
				SessionID: sess.id,
				Mobile:    sess.mobile,
				Status:    "need_captcha",
				Region:    "cn",
				Captcha: &CaptchaPrompt{
					Options: need.options,
					Reason:  "验证码已过期，请重新完成校验",
				},
			}, nil
		}
		return SendResult{}, err
	}
	if token == "" {
		return SendResult{}, stepErr("发送验证码", "上游未返回 state_token", nil)
	}
	sess.oneIDTok = token
	return SendResult{
		SessionID: sess.id,
		Mobile:    sess.mobile,
		Status:    status,
		ExpiresIn: expiresIn,
		Region:    "cn",
	}, nil
}

// tryAutoSolve 尝试自动过码。返回 nil 表示没成功，reason 说明原因。
//
// 刻意不返回 error：自动打码只是"省一步"，失败不该中断流程，
// 因为用户还可以自己在界面上过码。
func (m *Manager) tryAutoSolve(ctx context.Context, options CaptchaChallenge) (map[string]any, string) {
	opt, ok := options.PickSolvable()
	if !ok {
		return nil, "该号码要求的人机校验（teg）无法自动通过，请手动完成"
	}
	if m.solver == nil {
		return nil, "未配置打码平台密钥，请手动完成校验"
	}
	ticket, randStr, err := m.solver.Solve(ctx, opt.AppID)
	if err != nil {
		if errors.Is(err, ErrCaptchaBalance) {
			return nil, "打码平台余额不足，请手动完成校验"
		}
		return nil, "自动过码失败，请手动完成校验"
	}
	return map[string]any{
		"ticket":    ticket,
		"randStr":   randStr,
		"cloudType": opt.CloudType,
	}, ""
}

// Verify 校验短信验证码并走完剩余全部步骤，返回账号凭证。
//
// 会话保留策略（与上游实际行为对齐）：
//   - 验证码填错（E0010028）时上游**不会**作废 state_token，用户可以直接重填。
//     因此这种情况保留会话，不逼用户重新发短信。
//   - 验码已过、写票成功但 CLI 仍 11217：保留会话，允许空码重跑写票/取票。
//   - 其它 Keycloak/APISIX 失败仍丢弃会话。
func (m *Manager) Verify(ctx context.Context, sessionID, code string) (Credentials, error) {
	sessionID = strings.TrimSpace(sessionID)
	code = strings.TrimSpace(code)
	if sessionID == "" {
		return Credentials{}, ErrSessionNotFound
	}

	sess, err := m.claim(sessionID)
	if err != nil {
		return Credentials{}, err
	}
	// 默认丢弃；只有确认失败可原地重试时才改成保留。
	keep := false
	defer func() { m.release(sessionID, keep) }()

	// 写票成功但 CLI 还没出票时，会话里已经有 KEYCLOAK_IDENTITY。
	// 这时允许不带验证码再跑选账号/写票/取票，避免烧掉一条短信。
	if sess.verified && code == "" {
		creds, err := sess.finishAfterBroker(ctx, m)
		if err != nil {
			if isLoginPending(err) {
				keep = true
			}
			return Credentials{}, err
		}
		return creds, nil
	}

	// 还没过码就没有 state_token，此时验码只会拿到一个语焉不详的上游错误。
	// 明确告诉前端"先去完成校验"，并保留会话让它能接着做。
	if sess.oneIDTok == "" {
		keep = true
		return Credentials{}, stepErrRetryable("校验验证码", "请先完成人机校验", ErrCaptchaPending)
	}

	if code == "" {
		keep = true
		return Credentials{}, errors.New("验证码不能为空")
	}

	// 3) 验码。
	if err := sess.oneIDVerify(ctx, m.ep, code); err != nil {
		// 验证码填错可原地重填，保留会话（token 仍然有效）。
		var ue *Error
		if errors.As(err, &ue) && ue.Retryable {
			keep = true
		}
		return Credentials{}, err
	}
	sess.verified = true

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
	if err := sess.brokerFinish(ctx, m.ep, redirectURI, oneIDCode, brokerState); err != nil {
		return Credentials{}, err
	}

	creds, err := sess.finishAfterBroker(ctx, m)
	if err != nil {
		if isLoginPending(err) {
			keep = true
		}
		return Credentials{}, err
	}
	return creds, nil
}

// finishAfterBroker 从 Keycloak 身份开始：建 Console 会话、选账号、写票、取票。
// 写票成功但 CLI 仍是 11217 时由调用方保留会话，允许稍后空码重试这一段。
func (s *session) finishAfterBroker(ctx context.Context, m *Manager) (Credentials, error) {
	if err := s.establishConsoleSession(ctx, m.ep); err != nil {
		return Credentials{}, err
	}

	var ticket ticket
	var lastWrite string
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			if err := m.sleep(ctx, 500*time.Millisecond); err != nil {
				return Credentials{}, err
			}
		}
		consoleToken, loginErr := s.enterpriseLogin(ctx, m.ep)
		if loginErr != nil {
			return Credentials{}, loginErr
		}
		lastWrite, err = s.writeTicket(ctx, m.ep, consoleToken)
		if err != nil {
			return Credentials{}, err
		}
		ticket, err = s.fetchTicket(ctx, m)
		if err == nil {
			break
		}
		if !isLoginPending(err) {
			return Credentials{}, err
		}
	}
	if err != nil {
		return Credentials{}, stepErrRetryable("取回凭证",
			fmt.Sprintf("写票后 CLI 仍无凭证（11217）。写票跳转=%s，可复用当前会话重试取票", lastWrite), err)
	}

	account, err := s.lookupAccount(ctx, m.ep, ticket.AccessToken)
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
	consoleJar, err := newConsoleJar()
	if err != nil {
		return nil, err
	}
	oneIDJar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	s := &session{
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
			Transport:     wrapCookieFix(nil),
		},
		consoleNoJump: &http.Client{
			Timeout: 30 * time.Second, Jar: consoleJar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     wrapCookieFix(nil),
		},
		cliHTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirectLimit},
	}
	// 代理失败不该让登录直接挂掉：退回直连并记录原因，用户至少还能注册
	// （只是会占用本机 IP 的半小时额度）。
	if _, err := m.applyProxy(s); err != nil {
		return nil, err
	}
	return s, nil
}

func noRedirectLimit(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("重定向次数过多")
	}
	return nil
}

// claim 取出会话并标记占用。失败时用户仍可重填验证码，所以这里不删除会话；
// 由调用方在收尾时用 release 决定保留还是丢弃。
func (m *Manager) claim(id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrSessionNotFound
	}
	if s.busy {
		return nil, ErrSessionBusy
	}
	s.busy = true
	return s, nil
}

// release 归还会话。keep=true 表示会话仍然可用（例如验证码填错），保留给用户重试；
// keep=false 表示会话已作废，删除并擦除中间凭据。
func (m *Manager) release(id string, keep bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return
	}
	s.busy = false
	if !keep {
		s.close()
		delete(m.sessions, id)
	}
}

// store 保存新会话。重发验证码会签发新的 state_token，旧会话随之作废，
// 因此同号码的旧会话在这里一并清除，避免用户拿着失效的 session_id 反复重试。
func (m *Manager) store(s *session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked()
	for id, old := range m.sessions {
		if old.mobile == s.mobile {
			old.close()
			delete(m.sessions, id)
		}
	}
	m.sessions[s.id] = s
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
//
// verification 非 nil 时带上 captchaVerification —— 这是过完人机校验后的
// 第二次 send，上游据此把 status 变成 unexpired 并给出 state_token。
// 文档明确要求：不得伪造 ticket/randStr，必须真的过校验。
func (s *session) oneIDSend(ctx context.Context, ep Endpoints, verification map[string]any) (status string, expiresIn int64, token string, err error) {
	payload := map[string]any{
		"client_code": "codebuddy",
		"mobile":      s.mobile,
		"scopes":      []string{"openid", "mobile", "profile"},
	}
	if verification != nil {
		payload["captchaVerification"] = verification
	}
	raw, _ := json.Marshal(payload)
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, ep.OneID+"/v1/auth/sms/code/send", bytes.NewReader(raw))
	if reqErr != nil {
		return "", 0, "", reqErr
	}
	commonJSON(req)

	var resp oneIDSendResponse
	if err = s.doOneID(s.oneIDHTTP, req, &resp); err != nil {
		// 频控：距上次发码不足约 60 秒。此时旧验证码仍然有效，直接提示等待即可。
		if isOneIDCode(err, "E0010022") {
			return "", 0, "", stepErrRetryable("发送验证码", "发送太频繁，请等待倒计时结束后重试", ErrTooFrequent)
		}
		return "", 0, "", stepErr("发送验证码", "OneID 拒绝了本次请求", s.redactErr(err))
	}
	status = firstNonEmpty(resp.Status, resp.Data.Status)
	if challenge := s.captchaChallenge(resp); len(challenge) > 0 && status == "need_captcha" {
		// 把挑战带回给 Send 决定怎么过；这里不当作失败。
		return status, 0, "", &captchaRequired{options: challenge}
	}
	return status, firstNonZero(resp.ExpiresIn, resp.Data.ExpiresIn), firstNonEmpty(resp.StateToken, resp.Data.StateToken), nil
}

// captchaChallenge 取出上游要求的人机校验方案（兼容顶层与 data 两种位置）。
func (s *session) captchaChallenge(resp oneIDSendResponse) CaptchaChallenge {
	if len(resp.Captcha) > 0 {
		return resp.Captcha
	}
	return resp.Data.Captcha
}

// captchaRequired 是内部信号：本次 send 需要人机校验。
// 它不面向用户，Send 会捕获并尝试自动过码。
type captchaRequired struct {
	options CaptchaChallenge
}

func (e *captchaRequired) Error() string {
	return "需要人机校验"
}

// isOneIDCode 判断 OneID 错误是否携带指定 errCode。
func isOneIDCode(err error, code string) bool {
	return err != nil && strings.Contains(err.Error(), code)
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
		// E0010028 = 验证码错误：state_token 依然有效，用户可原地重填。
		// E0010022 = 操作太频繁：稍后重试即可，会话同样不受影响。
		if isOneIDCode(err, "E0010028") {
			return stepErrRetryable("校验验证码", "验证码不正确，请重新填写", ErrCodeWrong)
		}
		if isOneIDCode(err, "E0010022") {
			return stepErrRetryable("校验验证码", "操作太频繁，请稍等几秒再试", ErrTooFrequent)
		}
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
	navHeaders(req)
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
//
// 这是浏览器导航，不是 XHR。带 Origin / Accept: application/json 会被
// Keycloak / 网关当成异常请求直接 403。成功判据是 jar 里有 KEYCLOAK_IDENTITY，
// 最后一跳落在 /login HTML 上即使 403 也算过（页面本身不参与 API 登录）。
func (s *session) brokerFinish(ctx context.Context, ep Endpoints, redirectURI, code, state string) error {
	q := url.Values{}
	q.Set("code", code)
	q.Set("state", state)
	start := redirectURI
	if strings.Contains(start, "?") {
		start += "&" + q.Encode()
	} else {
		start += "?" + q.Encode()
	}
	// 对齐文档 curl -L：Keycloak 登录动作页声明 referrer-policy: no-referrer。
	// 带 Referer（尤其是带 code= 的上一跳）会被网关/WAF 直接 403，页面仍是
	// checkCookiesAndSetTimeout 那套登录主题，看起来像没带 Cookie。
	resp, err := s.followNavigation(ctx, start, "")
	if err != nil {
		return stepErr("完成登录", "Keycloak 回调失败", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if s.hasKeycloakIdentity(ep.Console) {
		return nil
	}
	finalURL := ""
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.Path
	}
	if resp.StatusCode >= 400 {
		reqURL := ""
		sent := ""
		if resp.Request != nil && resp.Request.URL != nil {
			reqURL = resp.Request.URL.String()
			sent = cookieHeaderNames(resp.Request.Header.Get("Cookie"))
		}
		detail := fmt.Sprintf("Keycloak 回调返回 HTTP %d（%s）", resp.StatusCode, finalURL)
		if reqURL != "" {
			detail += " url=" + redact(reqURL)
		}
		if sent != "" {
			detail += " sent=" + sent
		} else {
			detail += " sent=none"
		}
		if names := cookieNamesOn(s.consoleHTTP.Jar, reqURL); names != "" {
			detail += " jar=" + names
		}
		if snippet := htmlErrorSnippet(body); snippet != "" {
			detail += "：" + snippet
		}
		return stepErr("完成登录", detail, nil)
	}
	return stepErr("完成登录", "Keycloak 未写入登录会话（"+finalURL+"）", nil)
}

// followNavigation 手动跟随 302，每一跳用上一跳 URL 当 Referer。
// net/http 自动跟随会丢掉 Referer，Keycloak first-broker-login 因此 403。
func (s *session) followNavigation(ctx context.Context, start, referer string) (*http.Response, error) {
	current := start
	var last *http.Response
	for i := 0; i <= maxRedirects; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return nil, err
		}
		navHeaders(req)
		if isKeycloakPath(current) {
			req.Header.Del("Referer")
		} else if referer != "" {
			req.Header.Set("Referer", referer)
		}
		if isConsoleAPIPath(current) {
			req.Header.Set("X-Domain", consoleDomain)
		}
		resp, err := s.consoleNoJump.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode/100 != 3 {
			return resp, nil
		}
		loc := strings.TrimSpace(resp.Header.Get("Location"))
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		resp.Body.Close()
		if loc == "" {
			return nil, fmt.Errorf("HTTP %d 但没有 Location", resp.StatusCode)
		}
		next, err := resp.Request.URL.Parse(loc)
		if err != nil {
			return nil, fmt.Errorf("无效 Location %q: %w", loc, err)
		}
		forceHTTPSConsole(next)
		referer = navigationReferer(current)
		current = next.String()
		last = resp
	}
	if last != nil {
		last.Body.Close()
	}
	return nil, errors.New("重定向次数过多")
}

func (s *session) hasKeycloakIdentity(console string) bool {
	if s.consoleHTTP == nil || s.consoleHTTP.Jar == nil {
		return false
	}
	u, err := url.Parse(strings.TrimRight(console, "/") + "/auth/realms/copilot/")
	if err != nil {
		return false
	}
	for _, c := range s.consoleHTTP.Jar.Cookies(u) {
		if c.Name == "KEYCLOAK_IDENTITY" && strings.TrimSpace(c.Value) != "" {
			return true
		}
	}
	return false
}

// establishConsoleSession 第 8 步：访问 /console/accounts 触发 APISIX OIDC，
// 由网关用刚拿到的 Keycloak SSO 建立 /console 的 session Cookie。
func (s *session) establishConsoleSession(ctx context.Context, ep Endpoints) error {
	resp, err := s.followNavigation(ctx, ep.Console+"/console/accounts", "https://"+consoleDomain+"/")
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
//
// 前端真实形状（vendor bundle 的 oe/k 实现）：无请求体、无 Content-Type，
// 只带 X-Domain / X-Product-Code 与浏览器头，靠 APISIX session 认证。
// 返回的 token 只用于第 12 步查账号信息，不用于写票。
func (s *session) enterpriseLogin(ctx context.Context, ep Endpoints) (string, error) {
	u := ep.Console + "/console/login/enterprise?state=" + url.QueryEscape(s.cliState)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Origin", "https://"+consoleDomain)
	req.Header.Set("Referer", "https://"+consoleDomain+"/login/?platform=CLI&state="+url.QueryEscape(s.cliState))
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
// 判定口径（与 2026-09 手动全链基线实测一致，文档 4.8 的表是反的）：
//   - 请求必须带 APISIX session Cookie 且**不得**带 Authorization Bearer。
//     带 Bearer 会得到 302 Location=/ 的假成功，票实际没写上，取票永远 11217。
//   - 302 Location=/login?force_login_type=... 才是写票成功的真实信号。
//   - 302 Location=/ 表示没写上。
func (s *session) writeTicket(ctx context.Context, ep Endpoints, consoleToken string) (string, error) {
	q := url.Values{}
	q.Set("platform", "CLI")
	q.Set("state", s.cliState)
	q.Set("domain", consoleDomain)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.Console+"/console/auth/login?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	// 前端真实形状（vendor bundle 的 CacheToken 实现 ee/f）：
	// 只带 X-Domain 与浏览器导航头，靠 APISIX session Cookie 认证。
	// Authorization Bearer 一律不带——带了反而写不上。
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("X-Domain", consoleDomain)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", "https://"+consoleDomain+"/login/?platform=CLI&state="+url.QueryEscape(s.cliState))

	resp, err := s.consoleNoJump.Do(req)
	if err != nil {
		return "", stepErr("写入凭证", "写票请求失败", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	loc := strings.TrimSpace(resp.Header.Get("Location"))

	switch {
	case resp.StatusCode/100 == 3:
		if isTicketWrittenLocation(loc) {
			return loc, nil
		}
		return loc, stepErr("写入凭证", "写票未生效（Location="+loc+"）", nil)
	case resp.StatusCode == http.StatusOK:
		var env struct {
			Code int `json:"code"`
		}
		if json.Unmarshal(body, &env) == nil && env.Code == 0 {
			return "200", nil
		}
		return "200", stepErr("写入凭证", "写票响应异常", nil)
	default:
		return fmt.Sprintf("HTTP %d", resp.StatusCode), stepErr("写入凭证", fmt.Sprintf("写票返回 HTTP %d", resp.StatusCode), nil)
	}
}

// isTicketWrittenLocation 判断写票 302 的落点是否代表票已写上。
//
// 实测（17029646643 全链基线）：不带 Bearer、纯 APISIX session 写票时，
// Location 是 /login?force_login_type=&platform=CLI&state=...；带 Bearer 的
// 假成功是站点根 /。文档 4.8 的表把两者弄反了。
func isTicketWrittenLocation(loc string) bool {
	loc = strings.TrimSpace(loc)
	if loc == "" {
		return false
	}
	u, err := url.Parse(loc)
	if err != nil {
		return false
	}
	return strings.HasPrefix(u.Path, "/login")
}

type ticket struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// fetchTicket 第 11 步：取走写在该 state 上的一次性票。
//
// 文档写 CLI 轮询 GET /v2/plugin/auth/token。cmd/login 打 copilot.tencent.com，
// 登录页文档写 www.codebuddy.cn；两边都是同一套 /v2/plugin。先打 CLI 网关，
// 11217 时改打 Console 同源路径，并短轮询等写票落库。
func (s *session) fetchTicket(ctx context.Context, m *Manager) (ticket, error) {
	const attempts = 8
	var last error
	// 写票在 www.codebuddy.cn，票挂在这条出口上。先同域取；不行再打
	// copilot.tencent.com（cmd/login 的路径）。两边都 11217 才算没写上。
	bases := []string{m.ep.CLI}
	if m.ep.Console != "" && m.ep.Console != m.ep.CLI {
		bases = []string{m.ep.Console, m.ep.CLI}
	}
	for i := 0; i < attempts; i++ {
		if i > 0 {
			if err := m.sleep(ctx, 400*time.Millisecond); err != nil {
				return ticket{}, err
			}
		}
		for _, base := range bases {
			t, err := s.fetchTicketOnce(ctx, base)
			if err == nil {
				return t, nil
			}
			last = err
			if !isLoginPending(err) && !isNotFound(err) {
				return ticket{}, stepErr("取回凭证", "上游未返回账号令牌", err)
			}
		}
	}
	return ticket{}, stepErr("取回凭证", "上游未返回账号令牌", last)
}

func (s *session) fetchTicketOnce(ctx context.Context, base string) (ticket, error) {
	u := strings.TrimRight(base, "/") + "/v2/plugin/auth/token?state=" + url.QueryEscape(s.cliState)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ticket{}, err
	}
	// 对齐 cmd/login：Origin/Referer 指向 www.codebuddy.cn，UA 用 CLI。
	// 不要带登录 Cookie（文档：不要带登录 Cookie）。
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", "https://"+consoleDomain)
	req.Header.Set("Referer", "https://"+consoleDomain+"/")
	req.Header.Set("User-Agent", cliUA)
	req.Header.Set("X-No-Authorization", "true")

	client := s.cliHTTP
	if s.consoleHTTP != nil && looksLikeConsoleBase(base) {
		cp := *s.consoleHTTP
		cp.Jar = nil
		cp.CheckRedirect = noRedirectLimit
		client = &cp
	}

	var t ticket
	if err := s.doEnvelope(client, req, &t); err != nil {
		return ticket{}, err
	}
	if t.AccessToken == "" {
		return ticket{}, fmt.Errorf("code=11217 msg=11217:login ing...")
	}
	return t, nil
}

func looksLikeConsoleBase(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return strings.Contains(base, "codebuddy.cn")
	}
	host := strings.ToLower(u.Hostname())
	return host == consoleDomain || host == "codebuddy.cn" || strings.HasSuffix(host, ".codebuddy.cn")
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "HTTP 404") || strings.Contains(msg, "code=404")
}

func isLoginPending(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "11217") || strings.Contains(strings.ToLower(msg), "login ing")
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
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

// navHeaders 浏览器地址栏跳转用的头：不要 Origin，不要 JSON Accept。
// Keycloak broker / first-broker-login 是页面导航，带 XHR 头会被 WAF 403。
func navHeaders(req *http.Request) {
	req.Header.Del("Origin")
	req.Header.Del("Content-Type")
	req.Header.Del("X-Requested-With")
	req.Header.Del("X-Domain")
	req.Header.Del("Referer")
	req.Header.Del("Sec-Fetch-Dest")
	req.Header.Del("Sec-Fetch-Mode")
	req.Header.Del("Sec-Fetch-Site")
	req.Header.Del("Sec-Fetch-User")
	req.Header.Del("Upgrade-Insecure-Requests")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Del("Referer")
	req.Header.Del("X-Domain")
	req.Header.Del("Origin")
	req.Header.Del("X-Requested-With")
	req.Header.Del("Content-Type")
	req.Header.Del("Cookie")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("User-Agent", browserUA)
}

// isConsoleAPIPath 只有 /console/* 需要 X-Domain。带到 Keycloak /auth 上会被
// 网关当成异常头直接 403（实测卡在 first-broker-login）。
func isConsoleAPIPath(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return strings.Contains(rawURL, "/console/")
	}
	return strings.HasPrefix(u.Path, "/console/")
}

func isKeycloakPath(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return strings.Contains(rawURL, "/auth/realms/")
	}
	return strings.HasPrefix(u.Path, "/auth/")
}

func htmlErrorSnippet(body []byte) string {
	s := string(body)
	s = tagsRe.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	s = redact(s)
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) > 160 {
		s = string(runes[:160]) + "…"
	}
	return s
}

var tagsRe = regexp.MustCompile(`(?s)<[^>]*>`)

// doEnvelope 执行请求并解析业务 JSON。
//
// 两套上游形状都要认：
//   - CLI / Console 多数接口：{code,msg,data:{...}}
//   - Keycloak broker（from_oneid_login）：{code,state,redirect_uri}，字段在顶层、没有 data
//
// 只读 data 时，第二种会静默得到空字段，然后报「未返回 broker state」——
// 假上游把字段包进 data，单测绿、真登录翻车。data 为空时回退到整段 body。
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
	if out == nil {
		return nil
	}
	payload := env.Data
	if len(payload) == 0 || string(payload) == "null" {
		payload = body
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("解析 data 失败: %w", err)
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
	Status     string           `json:"status"`
	StateToken string           `json:"state_token"`
	ExpiresIn  int64            `json:"expires_in"`
	Captcha    CaptchaChallenge `json:"captcha"`
	Data       struct {
		Status     string           `json:"status"`
		StateToken string           `json:"state_token"`
		ExpiresIn  int64            `json:"expires_in"`
		Captcha    CaptchaChallenge `json:"captcha"`
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

// NationalNumber 从手机号里取出"国内号码"部分，用于和账号 nickname 比对。
// 中国区账号的 nickname 就是运营商号码本身（不含区号），例如 "+852 64087495"
// 对应 nickname "64087495"。无法识别成手机号时返回空串，避免把普通昵称
// 误当成号码匹配上。
func NationalNumber(mobile string) string {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, mobile)
	if len(digits) < 6 {
		return ""
	}
	// 已知区号：去掉后剩下的才是国内号码。要求剩余部分足够长，
	// 免得把以 86 开头的号码本身截断。
	for _, cc := range []string{"852", "853", "886", "86"} {
		if strings.HasPrefix(digits, cc) && len(digits)-len(cc) >= 6 {
			return digits[len(cc):]
		}
	}
	return digits
}

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
