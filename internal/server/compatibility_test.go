package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestBareBaseURLAliases(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{APIKey: "test-key", Pool: testPoolWith(&auth.Auth{UID: "u1", ExpiresAt: 9999999999}), Upstream: up})
	for _, tc := range []struct{ method, path, body, want string }{
		{"POST", "/chat/completions", `{"model":"glm-5.3","messages":[{"role":"user","content":"hello"}],"stream":true}`, "data: [DONE]"},
		{"POST", "/responses", `{"model":"glm-5.3","input":"hello","stream":true}`, "response.completed"},
		{"GET", "/models", "", `"object":"list"`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			denied := httptest.NewRecorder()
			h.ServeHTTP(denied, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			if denied.Code != http.StatusUnauthorized || !strings.Contains(denied.Body.String(), "invalid_api_key") {
				t.Fatalf("alias bypassed auth: %d %s", denied.Code, denied.Body)
			}
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer test-key")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("alias response: %d %s", rec.Code, rec.Body)
			}
		})
	}
}

type timeoutStreamReader struct{}

func (timeoutStreamReader) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }

func TestResponsesTimeoutDoesNotCompleteOrStoreHistory(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, "", true })
	up.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(io.MultiReader(strings.NewReader("data: {\"model\":\"glm-5.3\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"), timeoutStreamReader{})),
		}, nil
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", ExpiresAt: 9999999999}), Upstream: up})
	rec := doResponsesRequest(h, `{"model":"glm-5.3","input":"hello","stream":true}`)
	body := rec.Body.String()
	if !strings.Contains(body, "response.failed") || !strings.Contains(body, "upstream_timeout") || strings.Contains(body, "response.completed") {
		t.Fatalf("timeout was not reported as failure: %s", body)
	}
	if strings.Count(body, "data: [DONE]") != 1 || len(h.responseHistory) != 0 {
		t.Fatalf("timeout stored successful history or failed to terminate: history=%d body=%s", len(h.responseHistory), body)
	}
}
