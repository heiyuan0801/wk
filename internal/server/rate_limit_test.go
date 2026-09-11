package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestRateLimitResetAtParsesUTCOffset(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 9, 11, 9, 0, 0, 0, location)
	body := `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-11 17:58:44 UTC+8 重置，您也可以切换其他模型继续使用。"}`

	got, ok := rateLimitResetAt(body, now)
	if !ok {
		t.Fatal("reset time should be parsed")
	}
	want := time.Date(2026, 9, 11, 17, 58, 44, 0, location)
	if !got.Equal(want) {
		t.Fatalf("reset=%v want %v", got, want)
	}
	if !isExplicitRateLimit(body) {
		t.Fatal("code=6004 should be recognized as an explicit rate limit")
	}
}

func TestRateLimitResetAtParsesMinuteAndOffsetVariant(t *testing.T) {
	location := time.FixedZone("UTC+08:00", 8*60*60)
	now := time.Date(2026, 9, 11, 9, 0, 0, 0, location)
	body := `{"code":6004,"msg":"reset at 2026-09-11 18:00 UTC+08:00"}`

	got, ok := rateLimitResetAt(body, now)
	if !ok {
		t.Fatal("variant reset time should be parsed")
	}
	want := time.Date(2026, 9, 11, 18, 0, 0, 0, location)
	if !got.Equal(want) {
		t.Fatalf("reset=%v want %v", got, want)
	}
}

func TestRateLimitResetAtRejectsPastTimestamp(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 9, 11, 18, 0, 1, 0, location)
	body := `{"code":6004,"msg":"将在 2026-09-11 18:00:00 UTC+8 重置"}`
	if got, ok := rateLimitResetAt(body, now); ok || !got.IsZero() {
		t.Fatalf("past reset should be rejected, got %v ok=%v", got, ok)
	}
}

func TestRateLimitInfoHandlesWrapped503Body(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 9, 11, 9, 0, 0, 0, location)
	body := `status_code=503, all accounts unavailable (cooling/disabled): upstream soft_rate (http 429): {"code":6004,"msg":"将在 2026-09-11 17:58:44 UTC+8 重置"}`
	if !isExplicitRateLimit(body) {
		t.Fatal("wrapped 503 body should be recognized as an explicit rate limit")
	}
	if got, ok := rateLimitResetAt(body, now); !ok || got.Hour() != 17 || got.Minute() != 58 || got.Second() != 44 {
		t.Fatalf("wrapped reset=%v ok=%v", got, ok)
	}
}

func TestChatRateLimitUsesResetAndSkipsAccountUntilReset(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	reset := time.Now().Add(2 * time.Hour).In(location).Truncate(time.Second)
	body := fmt.Sprintf(`{"code":6004,"msg":"您的使用量已超出频率限制，将在 %s UTC+8 重置"}`, reset.Format("2006-01-02 15:04:05"))
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return http.StatusTooManyRequests, body, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})

	request := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[]}`)))
		return rec
	}
	if rec := request(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("first request should call the limited account once, calls=%d", calls)
	}

	status, ok := p.Status("u1")
	if !ok || !status.Cooling {
		t.Fatalf("account should be cooling: %+v ok=%v", status, ok)
	}
	if status.CoolKind != "rate_limit" {
		t.Fatalf("cool_kind=%q want rate_limit: %+v", status.CoolKind, status)
	}
	if delta := status.Until.Sub(reset); delta < -2*time.Second || delta > 2*time.Second {
		t.Fatalf("until=%v expected near reset=%v (delta=%v)", status.Until, reset, delta)
	}
	if !strings.Contains(status.Reason, "6004") || !strings.Contains(status.Reason, "reset_at=") {
		t.Fatalf("reason should expose reset metadata: %q", status.Reason)
	}

	// While the upstream window is active, a later request must fail locally
	// without selecting the limited account again.
	if rec := request(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("second code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("limited account was retried before reset, calls=%d", calls)
	}
}

func TestChatRateLimitWithoutResetUsesStrictFallback(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return http.StatusTooManyRequests, `{"code":6004,"msg":"rate limited"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Hour})
	request := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[]}`)))
		return rec
	}
	request()
	request()
	if calls != 1 {
		t.Fatalf("6004 without reset must still avoid retries, calls=%d", calls)
	}
	status, _ := p.Status("u1")
	if status.CoolKind != "rate_limit" || !strings.Contains(status.Reason, "reset time unavailable") {
		t.Fatalf("strict fallback status=%+v", status)
	}
}
