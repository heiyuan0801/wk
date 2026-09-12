// Package scheduler 定时任务：每日签到（09/21点）+ token keepalive（22点）。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/metricsstore"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool                    *pool.Pool
	Upstream                *upstream.Client
	CheckinHours            []int // 默认 [9, 21]
	KeepaliveHours          []int // 默认 [22]
	RequestCredits          metricsstore.Backend
	RequestLogRetentionDays int
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
	cleanupTicker := time.NewTicker(time.Hour)
	defer cleanupTicker.Stop()
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
		case <-cleanupTicker.C:
			s.RunRequestLogCleanupNow()
		}
	}
}

// UpdateRequestLogRetention applies a new retention value without restarting.
// A value of zero disables automatic deletion.
func (s *Scheduler) UpdateRequestLogRetention(days int) {
	if days < 0 || days > 3650 {
		return
	}
	s.mu.Lock()
	s.cfg.RequestLogRetentionDays = days
	s.mu.Unlock()
}

func (s *Scheduler) requestLogRetentionDays() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.RequestLogRetentionDays
}

// RunRequestLogCleanupNow deletes request details older than the configured
// retention period. Aggregate metrics and already reconciled credit totals are
// intentionally retained.
func (s *Scheduler) RunRequestLogCleanupNow() {
	days := s.requestLogRetentionDays()
	if days <= 0 || s.cfg.RequestCredits == nil {
		return
	}
	cleaner, ok := s.cfg.RequestCredits.(metricsstore.RequestLogCleaner)
	if !ok {
		return
	}
	deleted, err := cleaner.DeleteRequestLogsBefore(time.Now().Add(-time.Duration(days) * 24 * time.Hour))
	if err != nil {
		log.Printf("request log cleanup: %v", err)
		return
	}
	if deleted > 0 {
		log.Printf("request log cleanup: deleted %d records older than %d days", deleted, days)
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
	knownIDs := make(map[string]struct{}, len(logs))
	for _, record := range logs {
		if record.ID != "" {
			knownIDs[record.ID] = struct{}{}
		}
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
		const pageSize = 200
		for page := 1; ; page++ {
			rows, total, err := s.cfg.Upstream.UserRequestUsage(a, start, end, page, pageSize)
			if err != nil {
				log.Printf("request-usage %s: %v", st.UID, err)
				break
			}
			for _, row := range rows {
				if row.RequestID == "" {
					continue
				}
				matchedByID := false
				for _, id := range requestIDCandidates(row.RequestID) {
					if _, ok := knownIDs[id]; ok {
						matchedByID = true
					}
					if err := s.cfg.RequestCredits.ReconcileRequestCredit(id, row.Credit); err != nil {
						log.Printf("reconcile request credit %s: %v", id, err)
					}
				}
				if !matchedByID {
					if matcher, ok := s.cfg.RequestCredits.(metricsstore.RequestCreditTimeMatcher); ok {
						requestAt, err := time.ParseInLocation("2006-01-02 15:04:05", row.RequestTime, time.Local)
						if err == nil {
							if err := matcher.ReconcileRequestCreditByTime(st.UID, row.Model, requestAt, row.Credit); err != nil {
								log.Printf("reconcile request credit by time %s: %v", st.UID, err)
							}
						}
					}
				}
			}
			if len(rows) == 0 || total <= page*pageSize {
				break
			}
		}
	}
}

// requestIDCandidates accounts for WorkBuddy's billing meter using the crb-
// prefix while chat responses historically exposed the same identifier with
// a cmb- prefix. The original ID remains unchanged in downstream responses.
func requestIDCandidates(id string) []string {
	if id == "" {
		return nil
	}
	switch {
	case strings.HasPrefix(id, "crb-"):
		return []string{id, "cmb-" + strings.TrimPrefix(id, "crb-")}
	case strings.HasPrefix(id, "cmb-"):
		return []string{id, "crb-" + strings.TrimPrefix(id, "cmb-")}
	default:
		return []string{id}
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
		s.refreshAccountCredits(st.UID, a)
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
		s.refreshAccountCredits(st.UID, a)
	}
}

func (s *Scheduler) refreshAccountCredits(uid string, a *auth.Auth) {
	resource, err := s.cfg.Upstream.UserResourceDetails(a)
	if err != nil {
		log.Printf("user-resource %s: %v", uid, err)
		return
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
