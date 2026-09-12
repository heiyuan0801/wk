package smslogin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// CaptchaOption 是上游给出的一个人机校验方案。
//
// 注意 appId 不是登录页里写死的常量：腾讯换配置就会变，因此每次都以当次
// send 响应为准，不能缓存成固定值。
type CaptchaOption struct {
	AppID     string `json:"appId"`
	CloudType string `json:"cloudType"`
}

// CaptchaChallenge 兼容上游 captcha 字段出现过的所有形状：
// 字面量 null、单个对象、以及实测的数组。用自定义 UnmarshalJSON 而不是
// 固定类型，是因为上游历史上换过形状，而解析失败会导致整条登录链路中断。
type CaptchaChallenge []CaptchaOption

func (c *CaptchaChallenge) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*c = nil
		return nil
	}
	if trimmed[0] == '[' {
		var list []CaptchaOption
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		*c = list
		return nil
	}
	var one CaptchaOption
	if err := json.Unmarshal(trimmed, &one); err != nil {
		return err
	}
	*c = []CaptchaOption{one}
	return nil
}

// TwoCaptchaCloudType 是 2captcha 唯一吃得下的类型。
//
// 上游实测会同时给两条：teg（captcha.gtimg.com）与 tencent
// （turing.captcha.qcloud.com）。文档明确 teg 在 2captcha 没有对应 task，
// 因此只能挑 tencent 那条，别拿 teg 的 appId 去打。
const TwoCaptchaCloudType = "tencent"

// PickSolvable 按上游给出的顺序挑出可以交给 2captcha 的方案。
// 遇到 teg 直接跳过继续往下找；都不支持时返回 false，由调用方给出可读的失败原因。
func (c CaptchaChallenge) PickSolvable() (CaptchaOption, bool) {
	for _, opt := range c {
		if strings.EqualFold(opt.CloudType, TwoCaptchaCloudType) && strings.TrimSpace(opt.AppID) != "" {
			return opt, true
		}
	}
	return CaptchaOption{}, false
}

var (
	// ErrCaptchaNoKey 未配置打码平台密钥，无法自动过校验。
	ErrCaptchaNoKey = errors.New("未配置打码平台密钥")
	// ErrCaptchaUnsupported 上游只给了 2captcha 不支持的类型（如 teg）。
	ErrCaptchaUnsupported = errors.New("该号码要求的人机校验类型无法自动通过")
	// ErrCaptchaTimeout 打码超时。票据很短命，超时后需要重新拿挑战。
	ErrCaptchaTimeout = errors.New("人机校验超时")
	// ErrCaptchaBalance 打码平台余额不足（ERROR_ZERO_BALANCE）。
	ErrCaptchaBalance = errors.New("打码平台余额不足")
	// ErrCaptchaIncomplete 用户提交的验证码结果不完整（缺 ticket 或 randStr）。
	ErrCaptchaIncomplete = errors.New("人机校验结果不完整")
)

// Solver 解一次人机校验，返回可直接回灌 OneID 的票据。
// 抽象成接口是为了让测试不必真的去打码平台。
type Solver interface {
	Solve(ctx context.Context, appID string) (ticket, randStr string, err error)
}

// 登录页地址。文档说明 websiteURL 用登录页即可，不必带 CLI 的 state。
const captchaWebsiteURL = "https://www.codebuddy.cn/login/?platform=CLI"

// TwoCaptchaSolver 用 2captcha 解腾讯验证码。
//
// 只在 OneID 要求 need_captcha 时才会用到；未配置 ClientKey 时整个流程保持
// 原来的行为（提示用户改用浏览器授权），不会因为缺少密钥而报错。
type TwoCaptchaSolver struct {
	ClientKey string
	// Endpoint 允许测试指向本地假服务；空则用 2captcha 官方地址。
	Endpoint string
	// WebsiteURL 允许覆盖登录页地址；空则用 captchaWebsiteURL。
	WebsiteURL string
	// PollInterval 轮询间隔，默认 5s（文档建议 5–10 秒）。
	PollInterval time.Duration
	// Timeout 总超时，默认 120s。票据短命，超时后重来比死等更划算。
	Timeout time.Duration

	client *http.Client
	sleep  func(context.Context, time.Duration) error
}

// NewTwoCaptchaSolver 构造 solver。key 为空时返回 nil，调用方据此保持
// "未启用打码"的行为。
func NewTwoCaptchaSolver(key string) *TwoCaptchaSolver {
	if strings.TrimSpace(key) == "" {
		return nil
	}
	return &TwoCaptchaSolver{ClientKey: strings.TrimSpace(key)}
}

func (s *TwoCaptchaSolver) endpoint() string {
	if strings.TrimSpace(s.Endpoint) != "" {
		return strings.TrimRight(s.Endpoint, "/")
	}
	return "https://api.2captcha.com"
}

func (s *TwoCaptchaSolver) websiteURL() string {
	if strings.TrimSpace(s.WebsiteURL) != "" {
		return s.WebsiteURL
	}
	return captchaWebsiteURL
}

func (s *TwoCaptchaSolver) pollInterval() time.Duration {
	if s.PollInterval > 0 {
		return s.PollInterval
	}
	return 5 * time.Second
}

func (s *TwoCaptchaSolver) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return 120 * time.Second
}

func (s *TwoCaptchaSolver) httpClient() *http.Client {
	if s.client != nil {
		return s.client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (s *TwoCaptchaSolver) wait(ctx context.Context, d time.Duration) error {
	if s.sleep != nil {
		return s.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Solve 提交任务并轮询到 ready，返回 ticket/randStr。
func (s *TwoCaptchaSolver) Solve(ctx context.Context, appID string) (string, string, error) {
	if strings.TrimSpace(s.ClientKey) == "" {
		return "", "", ErrCaptchaNoKey
	}
	if strings.TrimSpace(appID) == "" {
		return "", "", ErrCaptchaUnsupported
	}

	taskID, err := s.createTask(ctx, appID)
	if err != nil {
		return "", "", err
	}

	deadline := time.Now().Add(s.timeout())
	for {
		if time.Now().After(deadline) {
			return "", "", ErrCaptchaTimeout
		}
		if err := s.wait(ctx, s.pollInterval()); err != nil {
			return "", "", err
		}
		sol, ready, err := s.fetchResult(ctx, taskID)
		if err != nil {
			return "", "", err
		}
		if !ready {
			continue
		}
		if strings.TrimSpace(sol.Ticket) == "" || strings.TrimSpace(sol.RandStr) == "" {
			return "", "", fmt.Errorf("打码平台返回的票据不完整")
		}
		return sol.Ticket, sol.RandStr, nil
	}
}

type twoCaptchaError struct {
	ErrorID     int    `json:"errorId"`
	ErrorCode   string `json:"errorCode"`
	Description string `json:"errorDescription"`
}

// classify 把 2captcha 的错误码映射成可读的失败原因。
func (e twoCaptchaError) classify() error {
	if e.ErrorID == 0 {
		return nil
	}
	switch e.ErrorCode {
	case "ERROR_ZERO_BALANCE":
		return fmt.Errorf("%w（%s）", ErrCaptchaBalance, e.Description)
	case "ERROR_WRONG_USER_KEY", "ERROR_KEY_DOES_NOT_EXIST",
		"ERROR_KEY_DOES_NOT_EXIST_OR_INCORRECT_FORMAT":
		return fmt.Errorf("%w：打码平台密钥无效（%s）", ErrCaptchaNoKey, e.Description)
	case "ERROR_TASK_NOT_SUPPORTED", "ERROR_APP_ID", "ERROR_INVALID_APPID":
		return fmt.Errorf("%w（%s）", ErrCaptchaUnsupported, e.Description)
	}
	msg := firstNonEmpty(e.ErrorCode, "打码平台返回错误")
	if e.Description != "" {
		msg += "：" + e.Description
	}
	return errors.New(msg)
}

func (s *TwoCaptchaSolver) createTask(ctx context.Context, appID string) (int64, error) {
	payload := map[string]any{
		"clientKey": s.ClientKey,
		"task": map[string]any{
			// Proxyless：不需要自备代理。TencentTask 必须额外带 proxy* 参数。
			"type":       "TencentTaskProxyless",
			"websiteURL": s.websiteURL(),
			"appId":      appID,
		},
	}
	var resp struct {
		twoCaptchaError
		TaskID int64 `json:"taskId"`
	}
	if err := s.post(ctx, "/createTask", payload, &resp); err != nil {
		return 0, err
	}
	if err := resp.classify(); err != nil {
		return 0, err
	}
	if resp.TaskID == 0 {
		return 0, errors.New("打码平台未返回 taskId")
	}
	return resp.TaskID, nil
}

type twoCaptchaSolution struct {
	AppID   string `json:"appid"`
	Ret     int    `json:"ret"`
	Ticket  string `json:"ticket"`
	RandStr string `json:"randstr"`
}

func (s *TwoCaptchaSolver) fetchResult(ctx context.Context, taskID int64) (twoCaptchaSolution, bool, error) {
	var resp struct {
		twoCaptchaError
		Status   string             `json:"status"`
		Solution twoCaptchaSolution `json:"solution"`
	}
	payload := map[string]any{"clientKey": s.ClientKey, "taskId": taskID}
	if err := s.post(ctx, "/getTaskResult", payload, &resp); err != nil {
		return twoCaptchaSolution{}, false, err
	}
	if err := resp.classify(); err != nil {
		return twoCaptchaSolution{}, false, err
	}
	switch resp.Status {
	case "ready":
		// ret != 0 表示腾讯侧判定失败，此时 ticket 不可用。
		if resp.Solution.Ret != 0 {
			return twoCaptchaSolution{}, false, fmt.Errorf("打码平台返回 ret=%d", resp.Solution.Ret)
		}
		return resp.Solution, true, nil
	case "processing":
		return twoCaptchaSolution{}, false, nil
	default:
		return twoCaptchaSolution{}, false, fmt.Errorf("打码平台返回未知状态 %q", resp.Status)
	}
}

func (s *TwoCaptchaSolver) post(ctx context.Context, path string, payload any, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint()+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("打码平台响应不是合法 JSON（HTTP %d）", resp.StatusCode)
	}
	return nil
}
