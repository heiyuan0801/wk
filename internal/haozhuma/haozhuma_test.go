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

// TestGetPhoneSendsAuthorAndISP 「[限对接]」项目必须带 author，运营商过滤
// 必须带 isp —— 实测缺 author 拿不到号，不限 isp 只会放广电号（收不到腾讯短信）。
func TestGetPhoneSendsAuthorAndISP(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write([]byte(`{"code":"0","msg":"成功","phone":"19572980371","uid":"52283-ABC"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New("tok")
	c.Base = u.String() + "/"
	c.Author = "adminzfz"
	c.ISP = "1"

	if _, err := c.GetPhone(context.Background(), "52283"); err != nil {
		t.Fatal(err)
	}
	if got.Get("author") != "adminzfz" {
		t.Errorf("author=%q want adminzfz", got.Get("author"))
	}
	if got.Get("isp") != "1" {
		t.Errorf("isp=%q want 1", got.Get("isp"))
	}
	// uid 从取号响应里学到，供后续收码用。
	if c.UID != "52283-ABC" {
		t.Errorf("uid=%q want 52283-ABC", c.UID)
	}
}

// TestGetPhoneOmitsEmptyFilters 普通项目（不配 author/isp）不该下发空参数。
func TestGetPhoneOmitsEmptyFilters(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write([]byte(`{"code":"0","msg":"成功","phone":"13800138000"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New("tok")
	c.Base = u.String() + "/"

	if _, err := c.GetPhone(context.Background(), "52283"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"author", "isp", "uid"} {
		if got.Has(k) {
			t.Errorf("param %s should be omitted when unset", k)
		}
	}
}

// TestGetPhoneISPFallback ISP 是优先级列表：前一档没号要退到下一档，
// 最后退回不限；不该因为移动号没货就整个取号失败。
func TestGetPhoneISPFallback(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isp := r.URL.Query().Get("isp")
		seen = append(seen, isp)
		// isp=1（移动）没号，其余有号。
		if isp == "1" {
			_, _ = w.Write([]byte(`{"code":"-1","msg":"没有取到号码，请重新尝试"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"0","msg":"成功","phone":"13900139000"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New("tok")
	c.Base = u.String() + "/"
	c.ISP = "1,2"

	phone, err := c.GetPhone(context.Background(), "52283")
	if err != nil {
		t.Fatalf("should fall back to next ISP, got %v", err)
	}
	if phone != "13900139000" {
		t.Fatalf("phone=%q", phone)
	}
	// 应该先试 1，再试 2 并成功（不该再多试）。
	if len(seen) < 2 || seen[0] != "1" || seen[1] != "2" {
		t.Fatalf("isp attempts=%v want [1 2]", seen)
	}
}

// TestGetPhoneISPListReachesUnrestricted 所有指定档都没号时退回"不限运营商"。
func TestGetPhoneISPListReachesUnrestricted(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isp := r.URL.Query().Get("isp")
		seen = append(seen, isp)
		if isp != "" {
			_, _ = w.Write([]byte(`{"code":"-1","msg":"没有取到号码，请重新尝试"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"0","msg":"成功","phone":"19200192000"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New("tok")
	c.Base = u.String() + "/"
	c.ISP = "1,2,3"

	phone, err := c.GetPhone(context.Background(), "52283")
	if err != nil {
		t.Fatal(err)
	}
	if phone != "19200192000" {
		t.Fatalf("phone=%q", phone)
	}
	if len(seen) != 4 || seen[3] != "" {
		t.Fatalf("isp attempts=%v want 1,2,3 then unrestricted", seen)
	}
}

// TestGetPhoneFatalErrorStopsFallback 余额不足这类致命错误不该继续试下一档。
func TestGetPhoneFatalErrorStopsFallback(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"code":"201","msg":"余额不足，请充值"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New("tok")
	c.Base = u.String() + "/"
	c.ISP = "1,2,3"

	if _, err := c.GetPhone(context.Background(), "52283"); err == nil {
		t.Fatal("want fatal error")
	}
	if calls != 1 {
		t.Fatalf("fatal error should stop after 1 call, got %d", calls)
	}
}

// TestNetworkErrorDoesNotLeakToken 网络错误绝不能把 token 带进日志：
// *url.Error 的 Error() 会拼出完整 URL（含 token=...）。
func TestNetworkErrorDoesNotLeakToken(t *testing.T) {
	const secret = "SUPERSECRETTOKEN1234567890"
	// 指向一个必然连不上的地址，触发 *url.Error。
	c := New(secret)
	c.Base = "http://127.0.0.1:1/"
	c.Timeout = 2 * time.Second

	_, err := c.GetPhone(context.Background(), "52283")
	if err == nil {
		t.Fatal("want network error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("token leaked into error: %v", err)
	}
	// 也不该泄露其它 query 参数拼成的完整 URL。
	if strings.Contains(err.Error(), "token=") {
		t.Fatalf("query leaked into error: %v", err)
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
