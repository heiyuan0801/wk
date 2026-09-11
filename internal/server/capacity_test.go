package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestStatusReportsModelSpecificCapacity(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "a"}, &auth.Auth{UID: "b"})
	p.SetMaxInFlight(3)
	p.CooldownModel("b", "kimi-k3", time.Minute, "test")
	if p.PickAndAcquireByUIDForModel("a", "kimi-k3") == nil {
		t.Fatal("missing lease")
	}
	defer p.Release("a")
	h := NewHandler(Config{Pool: p, APIKey: "secret"})
	denied := httptest.NewRecorder()
	h.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, "/status?model=kimi-k3-1", nil))
	if denied.Code != 401 {
		t.Fatal("capacity bypassed auth")
	}
	req := httptest.NewRequest(http.MethodGet, "/status?model=kimi-k3-1", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var body struct {
		Concurrency struct {
			Configured int `json:"configured_slots"`
			Available  int `json:"available_slots"`
			InFlight   int `json:"in_flight"`
		} `json:"concurrency"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || body.Concurrency.Configured != 3 || body.Concurrency.Available != 2 || body.Concurrency.InFlight != 1 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
}
