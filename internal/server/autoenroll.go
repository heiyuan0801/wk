package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/haozhuma"
	"workbuddy2api/internal/smslogin"
)

// AutoEnroller 自动加号：豪猪取号 → 本服务短信直登链 → 账号落盘。
//
// 一次 AutoRun(n) 串行跑 n 个号（代理池 cooldown 30 分钟决定不能并行刷）。
// 状态可通过 Status() 查询，方便控制台展示进度。
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
	attempts int
	ok       int
	fail     int
}

type AutoEnrollStatus struct {
	Running  bool     `json:"running"`
	Attempts int      `json:"attempts"`
	OK       int      `json:"ok"`
	Fail     int      `json:"fail"`
	Logs     []string `json:"logs"`
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
		Running:  a.running,
		Attempts: a.attempts,
		OK:       a.ok,
		Fail:     a.fail,
		Logs:     append([]string(nil), a.logs...),
	}
}

// AutoRun 对外入口。已在跑时返回错误。
func (a *AutoEnroller) AutoRun(n int) error {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return errors.New("自动加号已在进行中")
	}
	a.running = true
	a.attempts = 0
	a.ok = 0
	a.fail = 0
	a.logs = nil
	a.mu.Unlock()
	go func() {
		defer func() {
			a.mu.Lock()
			a.running = false
			a.mu.Unlock()
		}()
		a.run(n)
	}()
	return nil
}

// run 主循环。单个号失败不影响后续；达到成功数或耗尽尝试即停。
func (a *AutoEnroller) run(want int) {
	got := 0
	for i := 0; i < want*3 && got < want; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		ok, err := a.tryOne(ctx)
		cancel()
		a.mu.Lock()
		a.attempts++
		if ok {
			a.ok++
			got++
		} else {
			a.fail++
		}
		a.mu.Unlock()
		if err != nil {
			a.logf("第 %d 次尝试失败: %v", i+1, err)
		}
		// 两次尝试间稍微歇一下，别把代理池/上游打得过密。
		time.Sleep(5 * time.Second)
	}
	a.logf("结束：成功 %d / 尝试 %d（目标 %d）", a.ok, a.attempts, want)
}

// tryOne 走一个号的完整流程。返回是否成功加了号。
func (a *AutoEnroller) tryOne(ctx context.Context) (bool, error) {
	// 1) 豪猪取号。
	phone, err := a.hzm.GetPhone(ctx, a.sid)
	if err != nil {
		return false, fmt.Errorf("取号失败: %w", err)
	}
	a.logf("取号 %s", phone)

	release := func() { _ = a.hzm.Release(context.Background(), a.sid, phone) }
	blacklist := func() { _ = a.hzm.Blacklist(context.Background(), a.sid, phone) }

	// 2) 已在号池里的号直接释放换下一个（force=false 的 Send 会拒绝）。
	if a.find != nil {
		if _, exists := a.find(phone); exists {
			a.logf("号 %s 已在号池，释放换下一个", phone)
			release()
			return false, nil
		}
	}

	// 3) 发码（走本服务 SMSLogin：代理池 + 风控处理都在里面）。
	send, err := a.sms.Send(ctx, phone, "cn")
	if err != nil {
		var ue *smslogin.Error
		if errors.As(err, &ue) && ue.Retryable {
			a.logf("发码被拒（可重试）: %s", ue.Msg)
		} else {
			a.logf("发码失败: %v", err)
		}
		blacklist()
		release()
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
				release()
				return false, perr
			}
			a.logf("号 %s 加号成功 uid=%s…", phone, shortUID(creds.UID))
			release()
			return true, nil
		}
		a.logf("验码失败: %v", verr)
		blacklist()
		release()
		return false, verr
	}

	// 等不到码：拉黑（该号收不到腾讯短信）+ 释放。
	a.logf("号 %s 未收到验证码: %v", phone, waitErr)
	blacklist()
	release()
	return false, waitErr
}

// pollCode 轮询豪猪 getMessage，直到拿到验证码或短信过期。
// 腾讯验证码有效期 60s，这里轮 90s 上限（发码之后计数）。
// "等待"是正常轮询状态（豪猪返回 code=-1 msg=等待短信）——GetMessage 把它
// 当成功返回空 sms，这里只记轮次。
func (a *AutoEnroller) pollCode(ctx context.Context, phone string) (string, error) {
	deadline := time.Now().Add(90 * time.Second)
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
		case <-time.After(5 * time.Second):
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
