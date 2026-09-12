package haozhuma

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestExtractCode(t *testing.T) {
	cases := map[string]string{
		"【腾讯科技】验证码502612，用于手机登录，5分钟内有效": "502612",
		"您的验证码是 8432，请勿泄露":              "8432",
		"【CodeBuddy】你的验证码：998877":       "998877",
		"no code here": "",
		"":             "",
	}
	for sms, want := range cases {
		if got := ExtractCode(sms); got != want {
			t.Errorf("ExtractCode(%q)=%q want %q", sms, got, want)
		}
	}
}

// TestClientFlow 用假服务器验证 login/getPhone/getMessage/release 的请求形状
// 与"等待"状态的轮询语义。
func TestClientFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("api") {
		case "login":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","token":"tok-1"}`))
		case "getPhone":
			if r.URL.Query().Get("token") != "tok-1" || r.URL.Query().Get("sid") != "52283" {
				_, _ = w.Write([]byte(`{"code":"101","msg":"参数错误"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","phone":"17012345678"}`))
		case "getMessage":
			if r.URL.Query().Get("phone") != "17012345678" {
				_, _ = w.Write([]byte(`{"code":"102","msg":"号码不匹配"}`))
				return
			}
			// 不带 poll 参数时返回等待状态。
			if r.URL.Query().Get("poll") == "" {
				_, _ = w.Write([]byte(`{"code":"-1","msg":"等待短信"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","sms":"【腾讯科技】验证码502612，用于手机登录"}`))
		case "cancelRecv":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		default:
			_, _ = w.Write([]byte(`{"code":"404","msg":"unknown api"}`))
		}
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL + "/", Timeout: 5 * time.Second, HTTP: srv.Client()}

	// login
	if err := c.loginFlow(); err != nil {
		t.Fatalf("login: %v", err)
	}
	if c.Token != "tok-1" {
		t.Fatalf("token=%q", c.Token)
	}

	// getPhone
	phone, err := c.GetPhone(context.Background(), "52283")
	if err != nil || phone != "17012345678" {
		t.Fatalf("GetPhone: phone=%q err=%v", phone, err)
	}

	// getMessage：带 poll=2 的请求（服务器用它区分两次轮询）。
	q := url.Values{"token": {"tok-1"}, "sid": {"52283"}, "phone": {phone}, "poll": {"2"}}
	raw, err := c.call(context.Background(), "getMessage", q)
	if err != nil {
		t.Fatalf("second GetMessage: %v", err)
	}
	sms, _ := raw["sms"].(string)
	if code := ExtractCode(sms); code != "502612" {
		t.Fatalf("code from sms=%q", code)
	}

	// release
	if err := c.Release(context.Background(), "52283", phone); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// loginFlow 走 login 接口填 token（测试辅助）。
func (c *Client) loginFlow() error {
	raw, err := c.call(context.Background(), "login", url.Values{"user": {"u"}, "pass": {"p"}})
	if err != nil {
		return err
	}
	tok, _ := raw["token"].(string)
	c.Token = strings.TrimSpace(tok)
	return nil
}
