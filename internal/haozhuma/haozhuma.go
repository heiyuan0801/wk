package haozhuma

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client 调用豪猪接码平台 API（api.haozhuma.com/sms/?api=...）。
//
// 接口全部是 GET + query。code 字段："0"/"200" 成功；"-1" 且 msg 含
// "等待" 表示短信还没到（轮询用）；其它为失败。
type Client struct {
	Base    string
	Token   string
	Timeout time.Duration
	HTTP    *http.Client

	// user/pass 留着，token 失效时 Relogin 用。
	mu   sync.Mutex
	user string
	pass string

	// Author / UID 「[限对接]」项目的对接标识，官方 SDK 默认带 author=adminzfz。
	// 空字符串表示不下发（普通项目）。
	Author string
	UID    string

	// ISP 运营商过滤（取号参数 isp）。实测取值：1=移动 2=联通 3=电信。
	// 空字符串表示不限。腾讯短信通道对广电号支持极差，用 1 过滤掉。
	ISP string
}

func New(token string) *Client {
	return &Client{
		Base:    "https://api.haozhuma.com/sms/",
		Token:   token,
		Timeout: 15 * time.Second,
		HTTP:    &http.Client{},
	}
}

// NewWithCredentials 用已有 token 建客户端，同时记住账号密码，
// token 失效时可自动 Relogin。
func NewWithCredentials(token, user, password string) *Client {
	c := New(token)
	c.user = user
	c.pass = password
	return c
}

// Login 用账号密码换取 token。
func Login(user, password string) (*Client, error) {
	c := &Client{
		Base:    "https://api.haozhuma.com/sms/",
		Timeout: 15 * time.Second,
		HTTP:    &http.Client{},
	}
	raw, err := c.call(context.Background(), "login", url.Values{
		"user": {user}, "pass": {password},
	})
	if err != nil {
		return nil, err
	}
	token, _ := raw["token"].(string)
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("登录成功但未返回 token: %v", c.lastMsg(raw))
	}
	c.Token = token
	c.user = user
	c.pass = password
	return c, nil
}

// Relogin 用保存的账号密码重换 token（token 失效时调用）。
// 没存过凭据（token 直建模式）时返回错误。
func (c *Client) Relogin() error {
	c.mu.Lock()
	user, pass := c.user, c.pass
	c.mu.Unlock()
	if user == "" || pass == "" {
		return errors.New("token 失效且未保存账号密码，无法重登")
	}
	raw, err := c.call(context.Background(), "login", url.Values{
		"user": {user}, "pass": {pass},
	})
	if err != nil {
		return err
	}
	token, _ := raw["token"].(string)
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("重登成功但未返回 token")
	}
	c.mu.Lock()
	c.Token = token
	c.mu.Unlock()
	return nil
}

type Response map[string]any

func (r Response) Code() string {
	return anyCode(r)
}

func (r Response) Msg() string {
	v, _ := r["msg"].(string)
	return strings.TrimSpace(v)
}

func (r Response) Phone() string {
	v, _ := r["phone"].(string)
	return strings.TrimSpace(v)
}

func (r Response) SMS() string {
	v, _ := r["sms"].(string)
	return v
}

// code 字段有时是数字。统一取出来。
func anyCode(raw map[string]any) string {
	switch v := raw["code"].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return fmt.Sprintf("%g", v)
	default:
		return ""
	}
}

func (c *Client) lastMsg(raw map[string]any) string {
	v, _ := raw["msg"].(string)
	return v
}

// call 发一次请求并校验业务码。
func (c *Client) call(ctx context.Context, api string, params url.Values) (map[string]any, error) {
	u, err := url.Parse(c.Base)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("api", api)
	for k, vs := range params {
		for _, v := range vs {
			if strings.TrimSpace(v) == "" {
				continue
			}
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()

	ctx2, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx2, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// 绝不要把 err 原样往上报：*url.Error 会把完整 URL（含 token）
		// 拼进 Error()，一路带到日志里。
		return nil, fmt.Errorf("网络错误: api=%s: %w", api, redactNetErr(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	// token 失效时豪猪直接回 HTTP 403 且 body 不是 JSON（实测），
	// 这里必须当成"需要重登"而不是解析错误，否则任务会误判成普通失败。
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return nil, &APIError{API: api, Code: "403",
			Msg: fmt.Sprintf("HTTP %d（token 失效或账号被拒）", resp.StatusCode)}
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("响应不是 JSON（HTTP %d）: %s", resp.StatusCode, truncate(string(body), 200))
	}
	code := anyCode(raw)
	if code == "0" || code == "200" {
		return raw, nil
	}
	if code == "-1" && strings.Contains(c.lastMsg(raw), "等待") {
		return raw, nil
	}
	return raw, &APIError{API: api, Code: code, Msg: c.lastMsg(raw), Raw: raw}
}

type APIError struct {
	API  string
	Code string
	Msg  string
	Raw  map[string]any
}

func (e *APIError) Error() string {
	return fmt.Sprintf("豪猪[%s] code=%s msg=%s", e.API, e.Code, e.Msg)
}

// Waiting 报告是否处于"等待短信"状态（轮询场景的正常返回）。
func (e *APIError) Waiting() bool {
	return e.Code == "-1" && strings.Contains(e.Msg, "等待")
}

// Fatal 报告是否为继续重试也无意义的错误：账户余额不足、无号可取、项目
// 不存在、账号被禁。关键词取自实测响应（如错 sid 返回"没有找到项目ID"）。
//
// 注意区分两种"余额不足"：
//   - "您的余额不足,请释放拉黑后再取号" = 手里占用的号到了并发上限，
//     释放后就能继续取 —— 可恢复，不算致命（见 QuotaExhausted）。
//   - "余额不足，请充值" = 账户真没钱了 —— 致命。
func (e *APIError) Fatal() bool {
	if e == nil {
		return false
	}
	if e.QuotaExhausted() {
		return false
	}
	m := e.Msg
	for _, kw := range []string{
		"余额不足", "充值", "欠费",
		"无号", "没有可用", "暂无号码", "号码不足", "库存不足", "已售完",
		"项目不存在", "没有找到项目", "项目已", "项目被",
		"已被禁用", "封禁", "账号异常",
	} {
		if strings.Contains(m, kw) {
			return true
		}
	}
	switch e.Code {
	case "201", "202", "203", "204", "205", "301", "302", "303":
		// 常见豪猪错误段：201x 余额/套餐、30x 项目/账号被禁。
		return true
	}
	return false
}

// QuotaExhausted 报告是否为"占用号数到上限，先释放/拉黑再取"这类可恢复状态。
// 豪猪用「您的余额不足,请释放拉黑后再取号」表达这个意思——措辞有误导性，
// 实际与账户余额无关，是同时占用的号达到了上限。调用方应稍后重试。
func (e *APIError) QuotaExhausted() bool {
	if e == nil {
		return false
	}
	return strings.Contains(e.Msg, "释放") &&
		(strings.Contains(e.Msg, "余额不足") || strings.Contains(e.Msg, "再取号"))
}

// TokenInvalid 报告 token 是否失效（需要重新 login）。
// 实测 token 失效时豪猪返回 HTTP 403（无 JSON body），此处对应 Code=403。
func (e *APIError) TokenInvalid() bool {
	if e == nil {
		return false
	}
	if e.Code == "403" || e.Code == "401" {
		return true
	}
	if e.Code == "101" {
		return true
	}
	return strings.Contains(e.Msg, "token") || strings.Contains(e.Msg, "登录")
}

// Balance 返回 getSummary 里的余额（元）。解析失败返回 -1。
func (c *Client) Balance(ctx context.Context) (float64, error) {
	raw, err := c.call(ctx, "getSummary", url.Values{"token": {c.Token}})
	if err != nil {
		return -1, err
	}
	s, _ := raw["money"].(string)
	v, perr := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if perr != nil {
		if f, ok := raw["money"].(float64); ok {
			return f, nil
		}
		return -1, fmt.Errorf("余额字段无法解析: %v", raw["money"])
	}
	return v, nil
}

func (r Response) IsWaiting() bool { return false }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// GetPhone 在项目 sid 下取一个号。
//
// 「[限对接]」类项目按对接方放号，官方 SDK 会带 author（默认 adminzfz）
// 和 uid 参数；缺了会拿不到号（豪猪明确提示"建议学会加对接码后再取号"）。
//
// ISP 是运营商**优先级列表**（逗号分隔，1=移动 2=联通 3=电信）：依次尝试，
// 前一档没号就退到下一档；全部没号时才返回错误。实测同一项目往往只放
// 广电号（腾讯短信收不到），能挑到移动号时优先要移动。
func (c *Client) GetPhone(ctx context.Context, sid string) (string, error) {
	base := url.Values{"token": {c.Token}, "sid": {sid}}
	if c.Author != "" {
		base.Set("author", c.Author)
	}
	if c.UID != "" {
		base.Set("uid", c.UID)
	}

	var lastErr error
	for _, isp := range c.ispCandidates() {
		params := cloneValues(base)
		if isp != "" {
			params.Set("isp", isp)
		}
		raw, err := c.call(ctx, "getPhone", params)
		if err != nil {
			lastErr = err
			// 无号是"这一档没货"，继续试下一档；其它错误（余额/项目）直接抛。
			var ae *APIError
			if errors.As(err, &ae) && !ae.Fatal() {
				continue
			}
			return "", err
		}
		phone, _ := raw["phone"].(string)
		phone = strings.TrimSpace(phone)
		if phone == "" {
			lastErr = fmt.Errorf("取号成功但未返回手机号: %v", c.lastMsg(raw))
			continue
		}
		if u, _ := raw["uid"].(string); strings.TrimSpace(u) != "" && c.UID == "" {
			c.UID = strings.TrimSpace(u)
		}
		return phone, nil
	}
	if lastErr == nil {
		lastErr = errors.New("取号失败：没有可用号码")
	}
	return "", lastErr
}

// ispCandidates 展开 ISP 配置为优先级列表。空配置返回 [""]（不限运营商）。
func (c *Client) ispCandidates() []string {
	if strings.TrimSpace(c.ISP) == "" {
		return []string{""}
	}
	var out []string
	for _, part := range strings.Split(c.ISP, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	// 末尾补一个"不限"，保证前几档没号时仍能取到号（总比空手强）。
	return append(out, "")
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// redactNetErr 压掉网络错误里的完整 URL。*url.Error 的 Error() 形如
//
//	Get "https://...?token=<secret>": context canceled
//
// token 会随日志泄露，所以这里只保留方法、错误原因和主机名。
func redactNetErr(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	host := ""
	if u, perr := url.Parse(ue.URL); perr == nil {
		host = u.Host
	}
	inner := ue.Err
	if inner == nil {
		inner = errors.New("请求失败")
	}
	return fmt.Errorf("%s %s: %w", ue.Op, host, inner)
}

// GetMessage 查一次短信。返回 sms 原文与 code 状态。
// 官方 SDK 还会看 yzm 字段（有的项目把码单独放那里），这里两个都取。
func (c *Client) GetMessage(ctx context.Context, sid, phone string) (string, error) {
	raw, err := c.call(ctx, "getMessage", url.Values{
		"token": {c.Token}, "sid": {sid}, "phone": {phone},
	})
	if err != nil {
		return "", err
	}
	sms, _ := raw["sms"].(string)
	if strings.TrimSpace(sms) == "" {
		// 兜底：yzm 字段（SDK 优先用它）。
		if yzm, _ := raw["yzm"].(string); strings.TrimSpace(yzm) != "" {
			return yzm, nil
		}
	}
	return sms, nil
}

// Release 释放号码（cancelRecv）。
func (c *Client) Release(ctx context.Context, sid, phone string) error {
	_, err := c.call(ctx, "cancelRecv", url.Values{
		"token": {c.Token}, "sid": {sid}, "phone": {phone},
	})
	return err
}

// Blacklist 拉黑号码（收不到码的号）。
func (c *Client) Blacklist(ctx context.Context, sid, phone string) error {
	_, err := c.call(ctx, "addBlacklist", url.Values{
		"token": {c.Token}, "sid": {sid}, "phone": {phone},
	})
	return err
}

var codePatternAlt = regexp.MustCompile(`(\d{4,8})`)

// ExtractCode 从短信文本里抠验证码。腾讯注册短信通常是"验证码xxxxx"。
// 返回第一个 4-8 位数字串（优先"验证码"后面的）。
func ExtractCode(sms string) string {
	sms = strings.TrimSpace(sms)
	if sms == "" {
		return ""
	}
	// 优先 "验证码" 紧跟的数字。
	after := regexp.MustCompile(`验证码[^0-9]{0,6}([0-9]{4,8})`).FindStringSubmatch(sms)
	if len(after) >= 2 {
		return after[1]
	}
	// 兜底：任意独立 4-8 位数字。
	m := codePatternAlt.FindStringSubmatch(sms)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}
