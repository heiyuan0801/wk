package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLoginRegionSelection(t *testing.T) {
	h := NewHandler(Config{Region: "cn"})
	got, err := h.loginRegion(httptest.NewRequest("POST", "/admin/account/url?region=global", nil))
	if err != nil || got != "global" {
		t.Fatalf("global region=%q err=%v", got, err)
	}
	got, err = h.loginRegion(httptest.NewRequest("POST", "/admin/account/url?region=china", nil))
	if err != nil || got != "cn" {
		t.Fatalf("cn region=%q err=%v", got, err)
	}
	if _, err := h.loginRegion(httptest.NewRequest("POST", "/admin/account/url?region=all", nil)); err == nil {
		t.Fatal("all must not be accepted as a single login endpoint")
	}

	h.cfg.Region = "global"
	got, err = h.loginRegion(httptest.NewRequest("POST", "/admin/account/url", nil))
	if err != nil || got != "global" {
		t.Fatalf("configured global region=%q err=%v", got, err)
	}
}

func TestPromoteMixedRegionPersistsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"region":"cn","schedule":{"checkin_hours":[9]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{ConfigPath: path, Region: "cn"})
	if err := h.promoteMixedRegionIfNeeded("global"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if h.cfg.Region != "all" {
		t.Fatalf("runtime region=%q want all", h.cfg.Region)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["region"] != "all" || doc["schedule"] == nil {
		t.Fatalf("config after promote=%v", doc)
	}
	// Repeated calls are idempotent and must not rewrite the configuration.
	if err := h.promoteMixedRegionIfNeeded("global"); err != nil {
		t.Fatalf("second promote: %v", err)
	}
}
