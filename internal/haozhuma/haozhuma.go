package haozhuma

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
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
}

func New(token string) *Client {
	return &Client{
		Base:    "https://api.haozhuma.com/sms/",
		Token:   token,
		Timeout: 15 * time.Second,
		HTTP:    &http.Client{},
	}
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
	return c, nil
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
		return nil, fmt.Errorf("网络错误: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("响应不是 JSON: %s", truncate(string(body), 200))
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

// Waiting 报告是否处于"等待短信"状态。
func (e *APIError) Waiting() bool {
	return e.Code == "-1" && strings.Contains(e.Msg, "等待")
}

func (r Response) IsWaiting() bool { return false }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// GetPhone 在项目 sid 下取一个号。
func (c *Client) GetPhone(ctx context.Context, sid string) (string, error) {
	raw, err := c.call(ctx, "getPhone", url.Values{
		"token": {c.Token}, "sid": {sid},
	})
	if err != nil {
		return "", err
	}
	phone, _ := raw["phone"].(string)
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return "", fmt.Errorf("取号成功但未返回手机号: %v", c.lastMsg(raw))
	}
	return phone, nil
}

// GetMessage 查一次短信。返回 sms 原文与 code 状态。
func (c *Client) GetMessage(ctx context.Context, sid, phone string) (string, error) {
	raw, err := c.call(ctx, "getMessage", url.Values{
		"token": {c.Token}, "sid": {sid}, "phone": {phone},
	})
	if err != nil {
		return "", err
	}
	sms, _ := raw["sms"].(string)
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
