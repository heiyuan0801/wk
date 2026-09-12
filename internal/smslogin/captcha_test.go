package smslogin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fake2Captcha 复刻 2captcha 的 createTask / getTaskResult 行为。
type fake2Captcha struct {
	server *httptest.Server
	// createErr 非零时 createTask 直接返回该错误码。
	createErrCode string
	// readyAfter 是第几次 getTaskResult 才返回 ready。
	readyAfter int
	// solution 是 ready 时返回的解。
	ticket  string
	randStr string
	// ret 模拟腾讯侧判定失败。
	ret int
	// emptySolution 模拟票据不完整。
	emptySolution bool

	polls int
	// lastCreate 记录 createTask 收到的 task，供断言字段形状。
	lastCreate map[string]any
}

func newFake2Captcha(t *testing.T) *fake2Captcha {
	t.Helper()
	f := &fake2Captcha{readyAfter: 1, ticket: "tr03_ticket", randStr: "@randstr"}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAllLimited(r)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)

		switch r.URL.Path {
		case "/createTask":
			if task, ok := payload["task"].(map[string]any); ok {
				f.lastCreate = task
			}
			if f.createErrCode != "" {
				writeJSONRaw(w, 200, fmt.Sprintf(
					`{"errorId":1,"errorCode":%q,"errorDescription":"nope"}`, f.createErrCode))
				return
			}
			writeJSONRaw(w, 200, `{"errorId":0,"taskId":12345}`)
		case "/getTaskResult":
			f.polls++
			if f.polls < f.readyAfter {
				writeJSONRaw(w, 200, `{"errorId":0,"status":"processing"}`)
				return
			}
			if f.emptySolution {
				writeJSONRaw(w, 200, `{"errorId":0,"status":"ready","solution":{"ret":0,"ticket":"","randstr":""}}`)
				return
			}
			writeJSONRaw(w, 200, fmt.Sprintf(
				`{"errorId":0,"status":"ready","solution":{"appid":"197561220","ret":%d,"ticket":%q,"randstr":%q}}`,
				f.ret, f.ticket, f.randStr))
		default:
			writeJSONRaw(w, 404, `{"errorId":1,"errorCode":"ERROR_UNKNOWN"}`)
		}
	}))
	return f
}

func (f *fake2Captcha) Close() { f.server.Close() }

func (f *fake2Captcha) solver() *TwoCaptchaSolver {
	return &TwoCaptchaSolver{
		ClientKey:    "test-key",
		Endpoint:     f.server.URL,
		PollInterval: time.Millisecond,
		Timeout:      5 * time.Second,
	}
}

func readAllLimited(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 512)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}

// TestTwoCaptchaSolverSuccess 正常路径：createTask 用 TencentTaskProxyless，
// 轮询到 ready 后返回票据。
func TestTwoCaptchaSolverSuccess(t *testing.T) {
	f := newFake2Captcha(t)
	defer f.Close()

	ticket, randStr, err := f.solver().Solve(context.Background(), "197561220")
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	if ticket != "tr03_ticket" || randStr != "@randstr" {
		t.Fatalf("ticket=%q randStr=%q", ticket, randStr)
	}
	// 任务字段必须是 2captcha 认识的那套：Proxyless 不需要代理参数。
	if f.lastCreate["type"] != "TencentTaskProxyless" {
		t.Fatalf("task type=%v", f.lastCreate["type"])
	}
	if f.lastCreate["appId"] != "197561220" {
		t.Fatalf("appId=%v", f.lastCreate["appId"])
	}
	if f.lastCreate["websiteURL"] != captchaWebsiteURL {
		t.Fatalf("websiteURL=%v want %q", f.lastCreate["websiteURL"], captchaWebsiteURL)
	}
}

// TestTwoCaptchaSolverPollsUntilReady processing 时必须继续轮询而不是当成失败。
func TestTwoCaptchaSolverPollsUntilReady(t *testing.T) {
	f := newFake2Captcha(t)
	f.readyAfter = 3
	defer f.Close()

	if _, _, err := f.solver().Solve(context.Background(), "197561220"); err != nil {
		t.Fatalf("solve: %v", err)
	}
	if f.polls < 3 {
		t.Fatalf("should have polled at least 3 times, got %d", f.polls)
	}
}

// TestTwoCaptchaSolverBalanceError 余额不足要映射成可识别错误，便于给出可操作提示。
func TestTwoCaptchaSolverBalanceError(t *testing.T) {
	f := newFake2Captcha(t)
	f.createErrCode = "ERROR_ZERO_BALANCE"
	defer f.Close()

	_, _, err := f.solver().Solve(context.Background(), "197561220")
	if !errors.Is(err, ErrCaptchaBalance) {
		t.Fatalf("err=%v want ErrCaptchaBalance", err)
	}
}

// TestTwoCaptchaSolverBadKey 密钥无效要映射成 ErrCaptchaNoKey。
func TestTwoCaptchaSolverBadKey(t *testing.T) {
	f := newFake2Captcha(t)
	f.createErrCode = "ERROR_WRONG_USER_KEY"
	defer f.Close()

	_, _, err := f.solver().Solve(context.Background(), "197561220")
	if !errors.Is(err, ErrCaptchaNoKey) {
		t.Fatalf("err=%v want ErrCaptchaNoKey", err)
	}
}

// TestTwoCaptchaSolverRetNonZero ret != 0 表示腾讯侧判定失败，票据不可用。
func TestTwoCaptchaSolverRetNonZero(t *testing.T) {
	f := newFake2Captcha(t)
	f.ret = 1
	defer f.Close()

	if _, _, err := f.solver().Solve(context.Background(), "197561220"); err == nil {
		t.Fatal("ret != 0 must fail")
	}
}

// TestTwoCaptchaSolverIncompleteSolution 票据不完整时不能当成成功。
func TestTwoCaptchaSolverIncompleteSolution(t *testing.T) {
	f := newFake2Captcha(t)
	f.emptySolution = true
	defer f.Close()

	if _, _, err := f.solver().Solve(context.Background(), "197561220"); err == nil {
		t.Fatal("incomplete solution must fail")
	}
}

// TestTwoCaptchaSolverTimeout 打码超时要报 ErrCaptchaTimeout，由调用方提示重试。
func TestTwoCaptchaSolverTimeout(t *testing.T) {
	f := newFake2Captcha(t)
	f.readyAfter = 1000 // 永远不 ready
	defer f.Close()

	s := f.solver()
	s.Timeout = 30 * time.Millisecond
	s.PollInterval = time.Millisecond
	// 用真实 sleep 会太慢，注入一个不阻塞的等待。
	s.sleep = func(context.Context, time.Duration) error { return nil }

	_, _, err := s.Solve(context.Background(), "197561220")
	if !errors.Is(err, ErrCaptchaTimeout) {
		t.Fatalf("err=%v want ErrCaptchaTimeout", err)
	}
}

// TestTwoCaptchaSolverNoKey 未配置密钥时必须立刻失败，不发请求。
func TestTwoCaptchaSolverNoKey(t *testing.T) {
	s := &TwoCaptchaSolver{}
	if _, _, err := s.Solve(context.Background(), "197561220"); !errors.Is(err, ErrCaptchaNoKey) {
		t.Fatalf("err=%v want ErrCaptchaNoKey", err)
	}
}

// TestNewTwoCaptchaSolver 空 key 返回 nil，调用方据此保持"未启用打码"的行为。
func TestNewTwoCaptchaSolver(t *testing.T) {
	if s := NewTwoCaptchaSolver(""); s != nil {
		t.Fatal("empty key must yield nil solver")
	}
	if s := NewTwoCaptchaSolver("   "); s != nil {
		t.Fatal("blank key must yield nil solver")
	}
	s := NewTwoCaptchaSolver("k")
	if s == nil || s.ClientKey != "k" {
		t.Fatalf("solver=%+v", s)
	}
	// 默认端点与登录页地址要指向真实服务。
	if s.endpoint() != "https://api.2captcha.com" {
		t.Fatalf("endpoint=%q", s.endpoint())
	}
	if s.websiteURL() != captchaWebsiteURL {
		t.Fatalf("websiteURL=%q", s.websiteURL())
	}
}
