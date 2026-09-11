package server

import (
	"encoding/json"
	"fmt"
	"io"
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
	if !ok || status.Cooling {
		t.Fatalf("model-only limit must not cool the whole account: %+v ok=%v", status, ok)
	}
	modelLimit, ok := status.ModelCooldowns["deepseek-v4.1-flash"]
	if !ok {
		t.Fatalf("model cooldown missing: %+v", status)
	}
	if delta := modelLimit.Until.Sub(reset); delta < -2*time.Second || delta > 2*time.Second {
		t.Fatalf("model until=%v expected near reset=%v (delta=%v)", modelLimit.Until, reset, delta)
	}
	if !strings.Contains(modelLimit.Reason, "6004") || !strings.Contains(modelLimit.Reason, "reset_at=") || !strings.Contains(modelLimit.Reason, "model=deepseek-v4.1-flash") {
		t.Fatalf("model reason should expose reset metadata: %q", modelLimit.Reason)
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
	modelLimit, ok := status.ModelCooldowns["deepseek-v4.1-flash"]
	if status.Cooling || !ok || !strings.Contains(modelLimit.Reason, "reset time unavailable") {
		t.Fatalf("strict model fallback status=%+v", status)
	}
}

func TestChatRateLimitLeavesOtherModelAvailableOnSameAccount(t *testing.T) {
	const limitedModel = "deepseek-v4.1-flash"
	const otherModel = "deepseek-v4"
	reset := time.Now().Add(2 * time.Hour).In(time.FixedZone("UTC+8", 8*60*60)).Truncate(time.Second)
	body := fmt.Sprintf(`{"code":6004,"msg":"将在 %s UTC+8 重置"}`, reset.Format("2006-01-02 15:04:05"))
	var calls = map[string]int{}
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 500, "unused", false })
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		var request struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		calls[request.Model]++
		if request.Model == limitedModel {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseOK)),
		}, nil
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})

	request := func(model string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		payload := fmt.Sprintf(`{"model":%q,"messages":[]}`, model)
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload)))
		return rec
	}
	if rec := request(limitedModel); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("limited model code=%d body=%s", rec.Code, rec.Body)
	}
	if calls[limitedModel] != 1 {
		t.Fatalf("limited model should reach upstream once, calls=%d", calls[limitedModel])
	}
	if rec := request(otherModel); rec.Code != http.StatusOK {
		t.Fatalf("other model should use the same account, code=%d body=%s", rec.Code, rec.Body)
	}
	if calls[otherModel] != 1 {
		t.Fatalf("other model should reach upstream once, calls=%d", calls[otherModel])
	}
	if rec := request(limitedModel); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("limited model should stay blocked locally, code=%d body=%s", rec.Code, rec.Body)
	}
	if calls[limitedModel] != 1 {
		t.Fatalf("limited model was retried before reset, calls=%d", calls[limitedModel])
	}
}

func TestGenericRateLimitUsesModelScopeWhenModelPresent(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 500, "unused", false })
	up.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":"too many requests"}`)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseOK)),
		}, nil
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 2, SoftCooldown: time.Hour})

	request := func(model string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		payload := fmt.Sprintf(`{"model":%q,"messages":[]}`, model)
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload)))
		return rec
	}
	if rec := request("model-a"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("limited model code=%d body=%s", rec.Code, rec.Body)
	}
	if rec := request("model-b"); rec.Code != http.StatusOK {
		t.Fatalf("other model code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 2 {
		t.Fatalf("expected one upstream call per model, calls=%d", calls)
	}
	status, _ := p.Status("u1")
	if status.Cooling || len(status.ModelCooldowns) != 1 {
		t.Fatalf("generic 429 should leave account available and record one model limit: %+v", status)
	}
}

func TestRateLimitUsesCanonicalModelIDForAliases(t *testing.T) {
	reset := time.Now().Add(time.Hour).In(time.FixedZone("UTC+8", 8*60*60)).Truncate(time.Second)
	body := fmt.Sprintf(`{"code":6004,"msg":"将在 %s UTC+8 重置"}`, reset.Format("2006-01-02 15:04:05"))
	var calls int
	var upstreamModels []string
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 500, "unused", false })
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		var request struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		upstreamModels = append(upstreamModels, request.Model)
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 2})
	request := func(model string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		payload := fmt.Sprintf(`{"model":%q,"messages":[]}`, model)
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload)))
		return rec
	}
	if rec := request("kimi-k3-1"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("alias request code=%d body=%s", rec.Code, rec.Body)
	}
	if len(upstreamModels) != 1 || upstreamModels[0] != "kimi-k3" {
		t.Fatalf("upstream should receive canonical model, got %v", upstreamModels)
	}
	if rec := request("kimi-k3"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("canonical request code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("alias and canonical requests should share one cooldown, calls=%d", calls)
	}
	status, _ := p.Status("u1")
	if _, ok := status.ModelCooldowns["kimi-k3"]; !ok {
		t.Fatalf("canonical model cooldown missing: %+v", status)
	}
	if _, ok := status.ModelCooldowns["kimi-k3-1"]; ok {
		t.Fatalf("alias key should not create a separate cooldown: %+v", status)
	}
}

func TestResponsesRateLimitUsesModelScope(t *testing.T) {
	reset := time.Now().Add(time.Hour).In(time.FixedZone("UTC+8", 8*60*60)).Truncate(time.Second)
	body := fmt.Sprintf(`{"code":6004,"msg":"将在 %s UTC+8 重置"}`, reset.Format("2006-01-02 15:04:05"))
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return http.StatusServiceUnavailable, body, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 2})
	if rec := doResponsesRequest(h, `{"model":"deepseek-v4.1-flash","input":"hello"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("responses code=%d body=%s", rec.Code, rec.Body)
	}
	status, _ := p.Status("u1")
	if status.Cooling || len(status.ModelCooldowns) != 1 {
		t.Fatalf("Responses rate limit should be model-scoped: %+v", status)
	}
	if calls != 1 {
		t.Fatalf("Responses should not retry the limited model, calls=%d", calls)
	}
}
