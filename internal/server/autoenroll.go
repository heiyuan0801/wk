package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/haozhuma"
	"workbuddy2api/internal/smslogin"
)

// AutoEnroller 自动加号：豪猪取号 → 本服务短信直登链 → 账号落盘。
//
// 计费前提：豪猪只在**收码成功**时扣费，取号后收不到码不扣费。
// 因此下面的终止条件不是"省钱"，而是"别浪费时间/别在坏通道上打转"：
//   - 成功数达到目标
//   - 豪猪余额不足（低于单次成功成本时停：取到号也付不了款）
//   - 无号可取 / 项目不存在 / 账号被禁（重试无意义的致命错误）
//   - 连续失败达到熔断阈值（通道或接收率崩了，继续跑也不会有结果）
//   - token 失效：自动重登一次，重登后仍失败则终止
type AutoEnroller struct {
	sms     *smslogin.Manager
	hzm     *haozhuma.Client
	sid     string
	persist func(accountCredential, string) (map[string]any, int, error)
	find    func(mobile string) (nickname string, exists bool)

	mu      sync.Mutex
	running bool
	logs    []string
	started time.Time
	// stats
	attempts   int
	ok         int
	fail       int
	stopReason string
	workers    int

	// retryDelay 两次尝试之间的间隔（测试里调短）。0 时用默认 5s。
	retryDelay time.Duration
	// pollTimeout 单个号等验证码的时长（测试里调短）。0 时用默认 90s。
	pollTimeout time.Duration
	// pollInterval 轮询间隔（测试里调短）。0 时用默认 5s。
	pollInterval time.Duration

	// reloginMu 保证并发的 token 失效只触发一次重登。
	reloginMu   sync.Mutex
	lastRelogin time.Time
}

// 并发默认值与上限。并发加号靠代理池撑：每个号一个独立出口 IP
// （新会话创建时绑定一次）。上限保守——腾讯风控对同批次注册敏感，
// 并发太高会整批收不到码。
const (
	defaultWorkers = 3
	maxWorkers     = 8
)

// consecutiveFails 连续失败熔断阈值。单号收不到码很正常（接收率就是有概率），
// 但连续这么多个都失败说明通道坏了或项目被限，继续跑也出不了结果。
// 失败不扣费，所以阈值主要防"浪费时间"，可用 AUTO_ENROLL_MAX_CONSECUTIVE 覆盖。
var consecutiveFails = envInt("AUTO_ENROLL_MAX_CONSECUTIVE", 15)

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// minBalance 豪猪余额低于此值（元）时不再开始新的取号。只在**收码成功**时
// 扣费，所以留出够付一次成功的钱即可；比这更低时收到码也扣不了费，白等。
// 52283 腾讯项目单价 2.2 元/次，故默认 2.2；0 表示不做余额保护。
// 可用 AUTO_ENROLL_MIN_BALANCE 覆盖。
var minBalance = envFloat("AUTO_ENROLL_MIN_BALANCE", 2.2)

func envFloat(key string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return def
}

type AutoEnrollStatus struct {
	Running    bool     `json:"running"`
	Attempts   int      `json:"attempts"`
	OK         int      `json:"ok"`
	Fail       int      `json:"fail"`
	Workers    int      `json:"workers,omitempty"`
	StopReason string   `json:"stop_reason,omitempty"`
	Logs       []string `json:"logs"`
}

// NewAutoEnroller 组装自动加号器。persist 落盘回调必填（nil 时 tryOne 会
// 直接报错而不是 panic）；findAccountByMobile 可选，用于跳过已在池里的号。
func NewAutoEnroller(sms *smslogin.Manager, hzm *haozhuma.Client, sid string,
	persist func(accountCredential, string) (map[string]any, int, error),
	findAccountByMobile func(string) (string, bool)) *AutoEnroller {
	if persist == nil {
		persist = func(accountCredential, string) (map[string]any, int, error) {
			return nil, 500, errors.New("persist 回调未配置")
		}
	}
	if findAccountByMobile == nil {
		findAccountByMobile = func(string) (string, bool) { return "", false }
	}
	return &AutoEnroller{
		sms:     sms,
		hzm:     hzm,
		sid:     sid,
		persist: persist,
		find:    findAccountByMobile,
	}
}

func (a *AutoEnroller) logf(format string, args ...any) {
	msg := time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...)
	log.Printf("[auto-enroll] %s", msg)
	a.mu.Lock()
	a.logs = append(a.logs, msg)
	if len(a.logs) > 200 {
		a.logs = a.logs[len(a.logs)-100:]
	}
	a.mu.Unlock()
}

func (a *AutoEnroller) Status() AutoEnrollStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AutoEnrollStatus{
		Running:    a.running,
		Attempts:   a.attempts,
		OK:         a.ok,
		Fail:       a.fail,
		Workers:    a.workers,
		StopReason: a.stopReason,
		Logs:       append([]string(nil), a.logs...),
	}
}

// Balance 查当前豪猪余额（元）；查询失败返回 -1 和错误。
func (a *AutoEnroller) Balance(ctx context.Context) (float64, error) {
	return a.hzm.Balance(ctx)
}

// AutoRun 对外入口。已在跑时返回错误。workers 为并发数（<=0 用默认值）。
func (a *AutoEnroller) AutoRun(n, workers int) error {
	if workers <= 0 {
		workers = defaultWorkers
	}
	if workers > maxWorkers {
		workers = maxWorkers
	}
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return errors.New("自动加号已在进行中")
	}
	// 开跑前先看余额：不够一次成功就别启动，省得用户以为在跑其实取不到号。
	if bal, err := a.hzm.Balance(context.Background()); err == nil && bal >= 0 && bal < minBalance {
		a.mu.Unlock()
		return fmt.Errorf("豪猪余额不足（当前 %.2f 元，低于阈值 %.2f 元），请充值后再启动", bal, minBalance)
	}
	a.running = true
	a.attempts = 0
	a.ok = 0
	a.fail = 0
	a.logs = nil
	a.stopReason = ""
	a.workers = workers
	a.mu.Unlock()
	go func() {
		defer func() {
			a.mu.Lock()
			a.running = false
			a.mu.Unlock()
		}()
		reason := a.run(n, workers)
		a.mu.Lock()
		a.stopReason = reason
		a.mu.Unlock()
		ok, attempts := a.ok, a.attempts
		if reason != "" {
			a.logf("任务终止：%s", reason)
		} else {
			a.logf("完成：成功 %d / 尝试 %d（目标 %d，并发 %d）", ok, attempts, n, workers)
		}
	}()
	return nil
}

// run 并发主循环：workers 个 goroutine 各自串行地"取号→发码→收码→落盘"，
// 共享一个成功计数，达到目标数就一起停。
//
// 并发安全性：每个号在 SMSLogin 里是一个独立 session，创建时就绑定自己的
// 代理出口（applyProxy 在 newSession 里调一次），会话表有 mutex。所以并发
// 加号不会共用 IP、不会串号。
//
// 终止条件（任一 worker 触发即全体停止）：
//   - 成功数达到目标（正常完成）
//   - 豪猪余额不足 / 无号可取 / 项目不存在等致命错误
//   - 连续失败达到熔断阈值（跨 worker 累计——整个通道崩了）
//   - token 失效：重登一次（用 mutex 保证只登一次），再失败才停
func (a *AutoEnroller) run(want, workers int) string {
	const maxPerSuccess = 12
	limit := want * maxPerSuccess
	if limit < 20 {
		limit = 20
	}
	var (
		got         atomic.Int64 // 成功数
		used        atomic.Int64 // 已用尝试数
		consecutive atomic.Int64 // 连续失败（成功即清零）
		quotaWaits  atomic.Int64 // 因"占用号到上限"而退避的次数（诊断用）
		stopMu      sync.Mutex
		stopReason  string
	)
	setStop := func(reason string) {
		stopMu.Lock()
		if stopReason == "" {
			stopReason = reason
		}
		stopMu.Unlock()
	}
	stopped := func() bool {
		stopMu.Lock()
		defer stopMu.Unlock()
		return stopReason != ""
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for {
				if stopped() || got.Load() >= int64(want) {
					return
				}
				if used.Add(1) > int64(limit) {
					return
				}
				// 余额检查：所有 worker 都查一遍，代价可忽略（一次 HTTP），
				// 但能保证钱不够时立刻全停。
				if minBalance > 0 {
					bal, err := a.hzm.Balance(ctx)
					if err == nil && bal >= 0 && bal < minBalance {
						setStop(fmt.Sprintf("豪猪余额不足（%.2f 元 < %.2f），任务停止。请充值后重跑。", bal, minBalance))
						cancel()
						return
					}
				}
				tctx, tcancel := context.WithTimeout(ctx, 6*time.Minute)
				ok, err := a.tryOne(tctx, worker)
				tcancel()

				// 整体取消（另一 worker 已达目标/命中致命错误）导致的失败不算
				// 真实尝试：不计入统计，也不计入熔断。
				if !ok && errors.Is(err, context.Canceled) && ctx.Err() != nil {
					return
				}

				a.mu.Lock()
				a.attempts++
				if ok {
					a.ok++
				} else {
					a.fail++
				}
				attempts, oks := a.attempts, a.ok
				a.mu.Unlock()

				if ok {
					consecutive.Store(0)
					if got.Add(1) >= int64(want) {
						cancel()
						return
					}
				} else {
					var ae *haozhuma.APIError
					// 每次失败都记日志：并发时尤其需要看清是"取不到号"
					// 还是"收不到码"，否则熔断原因无从判断。
					if err != nil {
						a.logf("[w%d] 第 %d 次尝试失败: %v", worker, attempts, err)
					}
					// 占用号数到上限：等一会儿再取（别的 worker 释放后就有额度）。
					// 豪猪的措辞是"您的余额不足,请释放拉黑后再取号"，与账户
					// 余额无关，别当成致命错误终止整个任务。
					if errors.As(err, &ae) && ae.QuotaExhausted() {
						quotaWaits.Add(1)
						select {
						case <-ctx.Done():
							return
						case <-time.After(3 * time.Second):
						}
						continue
					}
					// 致命错误：余额/无号/项目禁用等，重试无意义。
					if errors.As(err, &ae) && ae.Fatal() {
						setStop(fmt.Sprintf("豪猪致命错误，任务停止：%v", err))
						cancel()
						return
					}
					// token 失效：重登一次再继续；重登失败则停。
					if errors.As(err, &ae) && ae.TokenInvalid() {
						a.reloginOnce()
						continue
					}
					if n := consecutive.Add(1); n >= int64(consecutiveFails) {
						setStop(fmt.Sprintf("连续 %d 个号失败（成功 %d/%d），熔断停止。若都是「取号失败」= 对接商没号；若是「未收到验证码」= 对接商号码收不到腾讯短信。失败号不扣费。",
							n, oks, want))
						cancel()
						return
					}
				}
				// 两次尝试间稍微歇一下，别把代理池/上游打得过密。
				delay := a.retryDelay
				if delay <= 0 {
					delay = 5 * time.Second
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
				}
			}
		}(w)
	}
	wg.Wait()
	stopMu.Lock()
	defer stopMu.Unlock()
	return stopReason
}

// reloginOnce 保证并发的 token 失效只触发一次重登。
func (a *AutoEnroller) reloginOnce() {
	a.reloginMu.Lock()
	defer a.reloginMu.Unlock()
	// 别的 worker 刚登过就不用再登（token 已经在几秒前刷新）。
	if time.Since(a.lastRelogin) < 30*time.Second {
		return
	}
	if err := a.hzm.Relogin(); err != nil {
		a.logf("豪猪 token 重登失败: %v", err)
		return
	}
	a.lastRelogin = time.Now()
	a.logf("豪猪 token 已重登，继续")
}

// tryOne 走一个号的完整流程。返回是否成功加了号。
// worker 只用于日志标记，方便并发时区分是哪个 worker 的动作。
//
// 号码处置：**所有**通过本流程取到的号最后都进黑名单——成功的号已经有账号了，
// 不该再发给我；失败/超时的号收不到腾讯短信，留着只会下次又被取到。
// 黑名单在 release 之前调用（豪猪要求先拉黑再释放）。
func (a *AutoEnroller) tryOne(ctx context.Context, worker int) (bool, error) {
	// 1) 豪猪取号。
	phone, err := a.hzm.GetPhone(ctx, a.sid)
	if err != nil {
		return false, fmt.Errorf("取号失败: %w", err)
	}
	a.logf("[w%d] 取号 %s", worker, phone)

	// finish 统一收尾：先拉黑再释放（豪猪 SDK 的顺序）。任何退出路径都要走它。
	finish := func() {
		bg := context.Background()
		_ = a.hzm.Blacklist(bg, a.sid, phone)
		_ = a.hzm.Release(bg, a.sid, phone)
	}

	// 2) 已在号池里的号：拉黑（避免反复取到同一个）+ 换下一个。
	if _, exists := a.find(phone); exists {
		a.logf("号 %s 已在号池，拉黑换下一个", phone)
		finish()
		return false, nil
	}

	// 3) 发码（走本服务 SMSLogin：代理池 + 风控处理都在里面）。
	send, err := a.sms.Send(ctx, phone, "cn")
	if err != nil {
		var ue *smslogin.Error
		if errors.As(err, &ue) && ue.Retryable {
			a.logf("号 %s 发码被拒（可重试）: %s", phone, ue.Msg)
		} else {
			a.logf("号 %s 发码失败: %v", phone, err)
		}
		finish()
		return false, err
	}
	sid := send.SessionID
	if len(sid) > 8 {
		sid = sid[:8]
	}
	a.logf("已发码 session=%s… 等短信", sid)

	// 4) 轮询豪猪收码（短信直登的验证码在 60s 内有效，轮询别超过）。
	code, waitErr := a.pollCode(ctx, phone)

	// 5) 验码（Verify 内部走完整 12 步并落凭据）。
	if waitErr == nil && code != "" {
		creds, verr := a.sms.Verify(ctx, send.SessionID, code)
		if verr == nil {
			// 6) 落盘。
			acc := accountCredential{
				UID:          creds.UID,
				Nickname:     creds.Nickname,
				EnterpriseID: creds.EnterpriseID,
				Domain:       creds.Domain,
				AccessToken:  creds.AccessToken,
				RefreshToken: creds.RefreshToken,
				ExpiresIn:    creds.ExpiresIn,
			}
			_, _, perr := a.persist(acc, creds.Region)
			if perr != nil {
				a.logf("号 %s 登录成功但落盘失败: %v", phone, perr)
				finish()
				return false, perr
			}
			a.logf("号 %s 加号成功 uid=%s…（已拉黑）", phone, shortUID(creds.UID))
			finish()
			return true, nil
		}
		a.logf("号 %s 验码失败: %v", phone, verr)
		finish()
		return false, verr
	}

	// 等不到码：拉黑 + 释放，换下一个号。
	a.logf("号 %s 未收到验证码: %v（已拉黑）", phone, waitErr)
	finish()
	return false, waitErr
}

// pollCode 轮询豪猪 getMessage，直到拿到验证码或短信过期。
// 腾讯验证码有效期 60s，这里轮 90s 上限（发码之后计数）。
// "等待"是正常轮询状态（豪猪返回 code=-1 msg=等待短信）——GetMessage 把它
// 当成功返回空 sms，这里只记轮次。
func (a *AutoEnroller) pollCode(ctx context.Context, phone string) (string, error) {
	wait := a.pollTimeout
	if wait <= 0 {
		wait = 90 * time.Second
	}
	deadline := time.Now().Add(wait)
	pollInterval := a.pollInterval
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}
	var polls int
	for time.Now().Before(deadline) {
		sms, err := a.hzm.GetMessage(ctx, a.sid, phone)
		if err != nil {
			var ae *haozhuma.APIError
			if ae != nil && ae.Waiting() {
				// 正常等待，继续轮。
			} else {
				return "", err
			}
		}
		polls++
		if code := haozhuma.ExtractCode(sms); code != "" {
			return code, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	return "", fmt.Errorf("验证码等待超时（轮询 %d 次未收到短信）", polls)
}

func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
