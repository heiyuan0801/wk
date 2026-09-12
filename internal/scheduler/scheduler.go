// Package scheduler 定时任务：每日签到（09/21点）+ token keepalive（22点）。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/metricsstore"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	KeepaliveHours []int // 默认 [22]
	RequestCredits metricsstore.Backend
}

// Scheduler 调度器。
type Scheduler struct {
	mu   sync.RWMutex
	cfg  Config
	wake chan struct{}
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	return &Scheduler{cfg: cfg, wake: make(chan struct{}, 1)}
}

// UpdateSchedule applies management-console changes without restarting the
// process and wakes Run so it can recalculate its next timer immediately.
func (s *Scheduler) UpdateSchedule(checkinHours, keepaliveHours []int) {
	if len(checkinHours) == 0 {
		checkinHours = []int{9, 21}
	}
	if len(keepaliveHours) == 0 {
		keepaliveHours = []int{22}
	}
	s.mu.Lock()
	s.cfg.CheckinHours = append([]int(nil), checkinHours...)
	s.cfg.KeepaliveHours = append([]int(nil), keepaliveHours...)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) schedule() (checkinHours, keepaliveHours []int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]int(nil), s.cfg.CheckinHours...), append([]int(nil), s.cfg.KeepaliveHours...)
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	creditTicker := time.NewTicker(5 * time.Minute)
	defer creditTicker.Stop()
	for {
		checkinHours, keepaliveHours := s.schedule()
		all := append(append([]int{}, checkinHours...), keepaliveHours...)
		next := nextFire(time.Now(), all)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
			continue
		case <-timer.C:
			h := time.Now().Hour()
			checkinHours, keepaliveHours = s.schedule()
			if contains(checkinHours, h) {
				s.RunCheckinNow()
			}
			if contains(keepaliveHours, h) {
				s.RunKeepaliveNow()
			}
		case <-creditTicker.C:
			s.RunRequestCreditRefreshNow()
		}
	}
}

// RunRequestCreditRefreshNow reconciles recent request logs with the
// authoritative web billing meter. It is intentionally asynchronous and never
// blocks an in-flight model request.
func (s *Scheduler) RunRequestCreditRefreshNow() {
	if s.cfg.RequestCredits == nil {
		return
	}
	logs, err := s.cfg.RequestCredits.RecentRequests(200)
	if err != nil {
		log.Printf("request usage logs: %v", err)
		return
	}
	if len(logs) == 0 {
		return
	}
	start := time.Now().Add(-48 * time.Hour)
	end := time.Now().Add(2 * time.Hour)
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().AccessToken == "" {
			continue
		}
		rows, _, err := s.cfg.Upstream.UserRequestUsage(a, start, end, 1, 200)
		if err != nil {
			log.Printf("request-usage %s: %v", st.UID, err)
			continue
		}
		for _, row := range rows {
			if row.RequestID != "" {
				_ = s.cfg.RequestCredits.ReconcileRequestCredit(row.RequestID, row.Credit)
			}
		}
	}
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().RefreshToken == "" {
			continue
		}
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			log.Printf("checkin %s: %v", st.UID, err)
			// 已签到等业务错误也继续走余额查询
		}
		_ = s.refreshAccountCredits(st.UID, a)
	}
}

// RunCreditRefreshNow refreshes upstream credit counters without performing a
// daily check-in. It is safe to run asynchronously during service startup.
func (s *Scheduler) RunCreditRefreshNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().RefreshToken == "" {
			continue
		}
		_ = s.refreshAccountCredits(st.UID, a)
	}
}

// refreshAccountCredits 拉取上游余额快照并写入账号池。
// 返回 error 供按账号调用方区分"签到成功但余额查询失败"。
func (s *Scheduler) refreshAccountCredits(uid string, a *auth.Auth) error {
	resource, err := s.cfg.Upstream.UserResourceDetails(a)
	if err != nil {
		log.Printf("user-resource %s: %v", uid, err)
		return err
	}
	s.cfg.Pool.SetCreditDetail(uid, pool.CreditDetail{
		Remaining:           resource.Remaining,
		CapacitySize:        resource.CapacitySize,
		CapacityRemain:      resource.CapacityRemain,
		CapacityUsed:        resource.CapacityUsed,
		CycleCapacitySize:   resource.CycleCapacitySize,
		CycleCapacityRemain: resource.CycleCapacityRemain,
		CycleCapacityUsed:   resource.CycleCapacityUsed,
	})
	return nil
}

// AccountResult 单个账号的签到/保活结果，供控制台按账号展示。
type AccountResult struct {
	UID    string
	OK     bool
	Detail string // 失败原因，OK 时为空
}

// CheckinAccount 对单个账号执行签到并刷新余额，供控制台"单账号签到"使用。
// 语义与 RunCheckinNow 的单账号分支完全一致：DailyCheckin 的业务错误
// （如"今日已签到"）不算失败，只要随后的余额查询成功即视为成功。
func (s *Scheduler) CheckinAccount(uid string) AccountResult {
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return AccountResult{UID: uid, Detail: "账号不存在"}
	}
	if a.Snapshot().RefreshToken == "" {
		return AccountResult{UID: uid, Detail: "账号缺少 refresh token"}
	}
	if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
		// 已签到等业务错误也继续走余额查询，仅记录日志。
		log.Printf("checkin %s: %v", uid, err)
	}
	if err := s.refreshAccountCredits(uid, a); err != nil {
		return AccountResult{UID: uid, Detail: err.Error()}
	}
	return AccountResult{UID: uid, OK: true}
}

// KeepaliveAccount 对单个账号刷新 token，供控制台"单账号保活"使用。
// 与 RunKeepaliveNow 的单账号分支一致：session 死亡时自动禁用该账号。
func (s *Scheduler) KeepaliveAccount(uid string) AccountResult {
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return AccountResult{UID: uid, Detail: "账号不存在"}
	}
	if a.Snapshot().RefreshToken == "" {
		return AccountResult{UID: uid, Detail: "账号缺少 refresh token"}
	}
	if err := s.cfg.Upstream.RefreshToken(a); err != nil {
		log.Printf("keepalive %s: %v", uid, err)
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
			s.cfg.Pool.Disable(uid, "12153 session dead")
		}
		return AccountResult{UID: uid, Detail: err.Error()}
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("keepalive %s save: %v", uid, err)
		return AccountResult{UID: uid, Detail: "凭证保存失败: " + err.Error()}
	}
	return AccountResult{UID: uid, OK: true}
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
func (s *Scheduler) RunKeepaliveNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().RefreshToken == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				s.cfg.Pool.Disable(st.UID, "12153 session dead")
			}
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", st.UID, err)
		}
	}
}
