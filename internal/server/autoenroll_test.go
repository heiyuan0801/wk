package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/haozhuma"
	"workbuddy2api/internal/smslogin"
)

// fakeHZM 假豪猪服务器：可编程的响应序列，用于测试终止逻辑。
type fakeHZM struct {
	t         *testing.T
	srv       *httptest.Server
	getPhone  atomic.Int32 // getPhone 调用次数
	summary   atomic.Value // string: 余额响应
	phoneResp atomic.Value // string: getPhone 响应
	tokenResp atomic.Value // string: login 响应
}

func newFakeHZM(t *testing.T) *fakeHZM {
	f := &fakeHZM{t: t}
	f.summary.Store(`{"code":"0","msg":"ok","money":"10.00"}`)
	f.phoneResp.Store(`{"code":"0","msg":"成功","phone":"17000000001"}`)
	f.tokenResp.Store(`{"code":"0","msg":"ok","token":"tok-1"}`)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Query().Get("api")
		switch api {
		case "login":
			_, _ = w.Write([]byte(f.tokenResp.Load().(string)))
		case "getSummary":
			_, _ = w.Write([]byte(f.summary.Load().(string)))
		case "getPhone":
			f.getPhone.Add(1)
			_, _ = w.Write([]byte(f.phoneResp.Load().(string)))
		case "getMessage":
			// 永远等待（模拟收不到码）。
			_, _ = w.Write([]byte(`{"code":"-1","msg":"等待短信"}`))
		case "cancelRecv", "addBlacklist":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		default:
			_, _ = w.Write([]byte(`{"code":"404","msg":"unknown"}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHZM) client() *haozhuma.Client {
	u, _ := url.Parse(f.srv.URL)
	c := haozhuma.New("tok-1")
	c.Base = u.String() + "/"
	return c
}

// successSMSManager 一个能走通完整短信直登链的 Manager（复用 sms_fake_test 的假上游）。
func successSMSManager(t *testing.T) *smslogin.Manager {
	t.Helper()
	up := newFakeSMSUpstream(t)
	t.Cleanup(up.Close)
	return smslogin.NewManager(up.Endpoints(), time.Minute)
}

// noSMS 一个永远发不出码的 SMSLogin 管理器（发码直接失败）。
func noSMSManager() *smslogin.Manager {
	// 用一个不存在的端点：Send 会因网络错误失败，但很快（连 localhost）。
	m := smslogin.NewManager(smslogin.Endpoints{
		Console: "http://127.0.0.1:1",
		CLI:     "http://127.0.0.1:1",
		OneID:   "http://127.0.0.1:1",
		Realm:   "copilot",
	}, time.Minute)
	return m
}

// TestAutoEnrollStopsOnFatalError 余额不足/无号可取等致命错误必须立即终止，
// 不能继续烧钱。
func TestAutoEnrollStopsOnFatalError(t *testing.T) {
	f := newFakeHZM(t)
	// 取号返回余额不足。
	f.phoneResp.Store(`{"code":"201","msg":"余额不足，请充值"}`)
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)

	if err := en.AutoRun(5, 1); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)
	st := en.Status()
	if st.Running {
		t.Fatal("task should be done")
	}
	if !strings.Contains(st.StopReason, "余额") {
		t.Fatalf("stop_reason=%q want 余额不足", st.StopReason)
	}
	// 致命错误只调一次 getPhone 就停。
	if n := f.getPhone.Load(); n > 2 {
		t.Fatalf("getPhone called %d times after fatal error, want <=2", n)
	}
}

// TestAutoEnrollStopsOnLowBalance 余额不足以支付一次成功取码时不该启动
// （开跑前先查余额，直接报错而不是跑起来才发现取不到号）。
func TestAutoEnrollStopsOnLowBalance(t *testing.T) {
	f := newFakeHZM(t)
	// 余额明显低于 minBalance 默认值（2.2，对应 52283 单价）。
	f.summary.Store(`{"code":"0","msg":"ok","money":"1.20"}`)
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = time.Millisecond

	err := en.AutoRun(3, 1)
	if err == nil {
		waitDone(t, en)
		t.Fatalf("low balance should refuse to start, status=%+v", en.Status())
	}
	if !strings.Contains(err.Error(), "余额不足") {
		t.Fatalf("err=%v want 余额不足", err)
	}
	// 拒绝启动时不该取号。
	if n := f.getPhone.Load(); n != 0 {
		t.Fatalf("getPhone called %d times, want 0", n)
	}
	// 也不该进入 running 状态。
	if en.Status().Running {
		t.Fatal("must not be marked running after refusing to start")
	}
}

// TestAutoEnrollCircuitBreaker 连续失败达到阈值必须熔断。
func TestAutoEnrollCircuitBreaker(t *testing.T) {
	f := newFakeHZM(t)
	// 取号正常，但 SMSLogin 用坏端点：每个号发码都失败 -> 连续失败。
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = time.Millisecond // 测试里别真等 5s

	if err := en.AutoRun(50, 1); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)
	st := en.Status()
	if !strings.Contains(st.StopReason, "连续") && !strings.Contains(st.StopReason, "熔断") {
		t.Fatalf("stop_reason=%q want 熔断", st.StopReason)
	}
	// 熔断阈值 consecutiveFails=8，远小于 50*3 的上限。
	if st.Attempts > consecutiveFails+2 {
		t.Fatalf("attempts=%d exceeds circuit breaker expectation", st.Attempts)
	}
	// 熔断必须真的省下取号：不该跑满 150 次。
	if n := f.getPhone.Load(); int(n) > consecutiveFails+2 {
		t.Fatalf("getPhone called %d times, circuit breaker did not stop it", n)
	}
}

// TestAutoEnrollReloginOnTokenInvalid token 失效时自动重登并继续。
func TestAutoEnrollReloginOnTokenInvalid(t *testing.T) {
	var phase atomic.Int32 // 0=token失效, 1=已重登
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("api") {
		case "login":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","token":"tok-2"}`))
		case "getSummary":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","money":"10.00"}`))
		case "getPhone":
			if phase.Load() == 0 {
				// 第一次：token 失效 -> 触发 Relogin。
				_, _ = w.Write([]byte(`{"code":"101","msg":"token失效请重新登录"}`))
				phase.Store(1)
				return
			}
			// 重登后：余额不足（致命错误，快速结束测试）。
			_, _ = w.Write([]byte(`{"code":"201","msg":"余额不足"}`))
		default:
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		}
	}))
	defer srv.Close()

	// 客户端必须记住账密，Relogin 才有凭据可用。
	u, _ := url.Parse(srv.URL)
	c := haozhuma.NewWithCredentials("tok-1", "user", "pass")
	c.Base = u.String() + "/"

	en := NewAutoEnroller(noSMSManager(), c, "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = time.Millisecond

	if err := en.AutoRun(2, 1); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)
	st := en.Status()
	// 重登后应继续到第二个致命错误（余额不足），而不是停在 token 失效。
	if !strings.Contains(st.StopReason, "余额") {
		t.Fatalf("stop_reason=%q want relogin then 余额不足", st.StopReason)
	}
	if c.Token != "tok-2" {
		t.Fatalf("token=%q want tok-2 (relogin)", c.Token)
	}
}

// TestReloginWithoutCredentials token 直建（无账密）时 Relogin 必须报错而不是静默失败。
func TestReloginWithoutCredentials(t *testing.T) {
	c := haozhuma.New("tok-only")
	if err := c.Relogin(); err == nil {
		t.Fatal("Relogin without credentials must fail")
	}
}

// TestAutoEnrollFatalErrorClassification 直接验证 APIError.Fatal 分类。
func TestAutoEnrollFatalErrorClassification(t *testing.T) {
	fatal := []string{
		`{"code":"201","msg":"余额不足，请先充值"}`,
		`{"code":"999","msg":"当前无号可取"}`,
		`{"code":"999","msg":"项目已被禁用"}`,
		// 实测量到的真实措辞：错 sid 返回这个。
		`{"code":"-1","msg":"没有找到项目ID"}`,
		`{"code":"999","msg":"号码库存不足"}`,
	}
	for _, raw := range fatal {
		// 用假服务器构造 APIError。
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(raw))
		}))
		u, _ := url.Parse(srv.URL)
		c := haozhuma.New("t")
		c.Base = u.String() + "/"
		_, err := c.GetPhone(context.Background(), "1")
		var ae *haozhuma.APIError
		if !errors.As(err, &ae) || !ae.Fatal() {
			t.Errorf("raw=%s should be fatal, got err=%v", raw, err)
		}
		srv.Close()
	}
	// 成功响应不是 fatal。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"0","msg":"ok","phone":"1"}`)
	}))
	u, _ := url.Parse(srv.URL)
	c := haozhuma.New("t")
	c.Base = u.String() + "/"
	if _, err := c.GetPhone(context.Background(), "1"); err != nil {
		var ae *haozhuma.APIError
		if errors.As(err, &ae) && ae.Fatal() {
			t.Error("success response must not be fatal")
		}
	}
	srv.Close()
}

// TestTokenInvalidOnHTTP403 实测 token 失效时豪猪回 HTTP 403 且 body 非 JSON，
// 必须被识别成 token 失效（触发重登）而不是普通的解析错误。
func TestTokenInvalidOnHTTP403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html>403 Forbidden</html>")
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := haozhuma.New("bad-token")
	c.Base = u.String() + "/"

	_, err := c.GetPhone(context.Background(), "52283")
	var ae *haozhuma.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("HTTP 403 must yield APIError, got %v", err)
	}
	if !ae.TokenInvalid() {
		t.Fatalf("HTTP 403 must be TokenInvalid, got code=%s msg=%s", ae.Code, ae.Msg)
	}
	if ae.Fatal() {
		t.Error("token invalid should not be classified fatal (relogin can fix it)")
	}
}

// TestBadSIDIsFatal 错项目 ID 实测返回 "没有找到项目ID"，必须终止任务。
func TestBadSIDIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"-1","data":null,"msg":"没有找到项目ID"}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := haozhuma.New("t")
	c.Base = u.String() + "/"

	_, err := c.GetPhone(context.Background(), "999999")
	var ae *haozhuma.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("bad sid must yield APIError, got %v", err)
	}
	if !ae.Fatal() {
		t.Fatalf("bad sid must be fatal, got code=%s msg=%s", ae.Code, ae.Msg)
	}
	if ae.TokenInvalid() {
		t.Error("bad sid must not be classified as token invalid (would retry forever via relogin)")
	}
}

// TestBalanceParsing 余额解析（真实响应 money 是字符串）。
func TestBalanceParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"money":"21.000","num":"300"}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := haozhuma.New("t")
	c.Base = u.String() + "/"

	bal, err := c.Balance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if bal != 21.0 {
		t.Fatalf("balance=%v want 21.0", bal)
	}
}

// TestAutoEnrollConcurrentRespectsTarget 并发跑时必须精确停在目标成功数，
// 不能超发（超发会多消耗号）。
func TestAutoEnrollConcurrentRespectsTarget(t *testing.T) {
	f := newFakeHZM(t)
	var seq atomic.Int64
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("api") {
		case "login":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","token":"tok-1"}`))
		case "getSummary":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","money":"100.00"}`))
		case "getPhone":
			n := seq.Add(1)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"code":"0","msg":"成功","phone":"1700000%04d"}`, n)))
		case "getMessage":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","sms":"验证码123456"}`))
		default:
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		}
	})
	en := NewAutoEnroller(successSMSManager(t), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		func(string) (string, bool) { return "", false },
	)
	en.retryDelay = time.Millisecond
	en.pollInterval = time.Millisecond

	const want = 6
	if err := en.AutoRun(want, 3); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)
	st := en.Status()
	if st.OK != want {
		t.Fatalf("ok=%d want exactly %d (overshoot wastes numbers)", st.OK, want)
	}
	if st.Workers != 3 {
		t.Fatalf("workers=%d want 3", st.Workers)
	}
}

// TestAutoEnrollBlacklistsEveryPhone 成功和失败的号都必须进黑名单——
// 成功的号已有账号不该再发，失败的号收不到码不该再被取到。
func TestAutoEnrollBlacklistsEveryPhone(t *testing.T) {
	f := newFakeHZM(t)
	var blacklisted, released sync.Map
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch q.Get("api") {
		case "login":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","token":"tok-1"}`))
		case "getSummary":
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","money":"100.00"}`))
		case "getPhone":
			n := f.getPhone.Add(1)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"code":"0","msg":"成功","phone":"1700000%04d"}`, n)))
		case "getMessage":
			// 尾号偶数收不到码（失败），奇数给码（成功）。
			p := q.Get("phone")
			if (p[len(p)-1]-'0')%2 == 0 {
				_, _ = w.Write([]byte(`{"code":"-1","msg":"等待短信"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok","sms":"验证码123456"}`))
		case "addBlacklist":
			blacklisted.Store(q.Get("phone"), true)
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		case "cancelRecv":
			released.Store(q.Get("phone"), true)
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		default:
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		}
	})
	en := NewAutoEnroller(successSMSManager(t), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		func(string) (string, bool) { return "", false },
	)
	en.retryDelay = time.Millisecond
	en.pollInterval = time.Millisecond
	en.pollTimeout = 50 * time.Millisecond // 失败号快速超时

	if err := en.AutoRun(3, 1); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)

	var nBlack, nRel int
	blacklisted.Range(func(k, _ any) bool { nBlack++; return true })
	released.Range(func(k, _ any) bool { nRel++; return true })
	if nBlack == 0 {
		t.Fatal("nothing was blacklisted")
	}
	// 每一个取到过的号都要既拉黑又释放。
	if nBlack != nRel {
		t.Fatalf("blacklisted=%d released=%d, must match", nBlack, nRel)
	}
	if n := int(f.getPhone.Load()); nBlack != n {
		t.Fatalf("blacklisted=%d but took %d numbers, every taken number must be blacklisted", nBlack, n)
	}
}

// TestAutoEnrollConcurrentCircuitBreaker 并发下熔断阈值仍然生效。
func TestAutoEnrollConcurrentCircuitBreaker(t *testing.T) {
	f := newFakeHZM(t)
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = time.Millisecond

	if err := en.AutoRun(50, 4); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)
	st := en.Status()
	if st.StopReason == "" {
		t.Fatalf("concurrent run should stop with a reason, status=%+v", st)
	}
	// 并发不会让熔断失效：尝试次数被限制住，不会跑满 50*12。
	if st.Attempts > consecutiveFails+4*2 {
		t.Fatalf("attempts=%d too high for circuit breaker", st.Attempts)
	}
}

// TestQuotaExhaustedIsNotFatal 豪猪的「您的余额不足,请释放拉黑后再取号」
// 是"占用号数到上限"的意思，不是账户没钱——绝不能终止整个任务。
func TestQuotaExhaustedIsNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"-1","msg":"您的余额不足,请释放拉黑后再取号"}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := haozhuma.New("t")
	c.Base = u.String() + "/"

	_, err := c.GetPhone(context.Background(), "52283")
	var ae *haozhuma.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want APIError, got %v", err)
	}
	if !ae.QuotaExhausted() {
		t.Fatalf("should be QuotaExhausted, msg=%q", ae.Msg)
	}
	if ae.Fatal() {
		t.Fatalf("quota exhaustion must NOT be fatal (task should wait and retry), msg=%q", ae.Msg)
	}
}

// TestRealBalanceInsufficientIsFatal 账户真没钱时必须终止。
func TestRealBalanceInsufficientIsFatal(t *testing.T) {
	for _, msg := range []string{"余额不足，请充值", "您的余额不足，请充值后使用"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"code":"201","msg":%q}`, msg)
		}))
		u, _ := url.Parse(srv.URL)
		c := haozhuma.New("t")
		c.Base = u.String() + "/"
		_, err := c.GetPhone(context.Background(), "52283")
		var ae *haozhuma.APIError
		if !errors.As(err, &ae) || !ae.Fatal() {
			t.Errorf("msg=%q should be fatal, got %v", msg, err)
		}
		if ae != nil && ae.QuotaExhausted() {
			t.Errorf("msg=%q must not be QuotaExhausted", msg)
		}
		srv.Close()
	}
}

// TestAutoEnrollStop 运行中必须能被停掉（否则只能重启容器）。
// 刻意用"取号成功但永远收不到码"：任务会一直轮询，只有 Stop 能结束它。
func TestAutoEnrollStop(t *testing.T) {
	f := newFakeHZM(t)
	// getMessage 永远返回"等待"，pollTimeout 设很长，任务因此一直挂着。
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = 10 * time.Millisecond
	en.pollInterval = 20 * time.Millisecond
	en.pollTimeout = 60 * time.Second // 故意很长：证明是 Stop 生效而不是等超时

	// noSMSManager 发码会立刻失败，任务瞬间跑完；这里换成一个"发码成功但
	// 收不到码"的假上游，让任务停在收码轮询上。
	en.sms = successSMSManager(t)

	if err := en.AutoRun(10, 2); err != nil {
		t.Fatal(err)
	}
	// 等它真的进入收码轮询。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !en.Status().Running {
		time.Sleep(10 * time.Millisecond)
	}
	if !en.Status().Running {
		t.Fatal("task should be running")
	}

	if !en.Stop("测试停止") {
		t.Fatal("Stop should report true while running")
	}
	// 必须在很短时间内结束（远小于 60s 的 pollTimeout，证明是取消起作用）。
	done := make(chan struct{})
	go func() { waitDone(t, en); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not finish the task in time")
	}
	if en.Status().Running {
		t.Fatal("should not be running after Stop")
	}
	// 没有正在跑的任务时 Stop 返回 false，而不是报错。
	if en.Stop("") {
		t.Fatal("Stop on idle task should report false")
	}
}

// TestAutoEnrollStopKeepsReason Stop 之后轮询仍能读到终止原因。
func TestAutoEnrollStopKeepsReason(t *testing.T) {
	f := newFakeHZM(t)
	en := NewAutoEnroller(noSMSManager(), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = 10 * time.Millisecond
	en.pollInterval = 10 * time.Millisecond
	en.pollTimeout = 60 * time.Second

	if err := en.AutoRun(10, 1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !en.Status().Running {
		time.Sleep(10 * time.Millisecond)
	}
	en.Stop("用户手动停止")
	waitDone(t, en)
	if got := en.Status().StopReason; !strings.Contains(got, "停止") {
		t.Fatalf("stop_reason=%q want 手动停止", got)
	}
}

// TestAutoEnrollStopCountsConsumedNumbers 中途停止时，已经取走的号必须
// 计入消耗——否则"取了 3 个号后停止"会显示成 0，看不出号码去了哪。
func TestAutoEnrollStopCountsConsumedNumbers(t *testing.T) {
	f := newFakeHZM(t)
	en := NewAutoEnroller(successSMSManager(t), f.client(), "52283",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil },
		nil)
	en.retryDelay = 10 * time.Millisecond
	en.pollInterval = 20 * time.Millisecond
	en.pollTimeout = 60 * time.Second // 卡在收码轮询，等 Stop

	if err := en.AutoRun(10, 2); err != nil {
		t.Fatal(err)
	}
	// 等两个 worker 都取到号（getPhone 被调用 >= 2 次）。
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if f.getPhone.Load() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if f.getPhone.Load() < 2 {
		t.Fatalf("workers should have taken numbers, getPhone=%d", f.getPhone.Load())
	}

	en.Stop("测试停止")
	waitDone(t, en)

	st := en.Status()
	taken := int(f.getPhone.Load())
	if st.Consumed == 0 {
		t.Fatalf("consumed=0 but %d numbers were taken — consumption must be reported", taken)
	}
	// 每个取到的号都应被计入（允许任务在 Stop 前多取，所以用 >=）。
	if st.Consumed < 2 {
		t.Fatalf("consumed=%d want >=2 (getPhone called %d times)", st.Consumed, taken)
	}
	// 被取消的尝试不该被算成失败。
	if st.Fail != 0 {
		t.Fatalf("fail=%d, cancelled attempts must not count as failures", st.Fail)
	}
}

func waitDone(t *testing.T, en *AutoEnroller) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if !en.Status().Running {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("auto-enroll did not finish in 30s")
}
