package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveLoginRegion(t *testing.T) {
	t.Setenv("WB2A_LOGIN_REGION", "")
	t.Setenv("WB2A_REGION", "all")
	t.Setenv("WB2A_LOGIN_PORTAL", "")
	t.Setenv("WB2A_LOGIN_STATE_FILE", "")

	cn, err := resolveLoginRegion("")
	if err != nil {
		t.Fatalf("resolve cn: %v", err)
	}
	if cn.Name != "cn" || cn.BaseURL != upstreamBaseCN || cn.Origin != originRefererCN {
		t.Fatalf("cn config=%+v", cn)
	}
	if cn.Portal != "codebuddy" {
		t.Fatalf("default cn portal=%q", cn.Portal)
	}
	t.Setenv("WB2A_LOGIN_PORTAL", "workbuddy")
	work, err := resolveLoginRegion("cn")
	if err != nil {
		t.Fatalf("resolve workbuddy portal: %v", err)
	}
	if work.Portal != "workbuddy" || work.Origin != originRefererWorkCN {
		t.Fatalf("workbuddy config=%+v", work)
	}
	if got := rewriteLoginURL("https://copilot.tencent.com/login?platform=CLI&state=s1", work.Portal); got != "https://www.workbuddy.cn/login?platform=CLI&state=s1" {
		t.Fatalf("rewritten login URL=%q", got)
	}
	t.Setenv("WB2A_LOGIN_PORTAL", "")

	global, err := resolveLoginRegion("overseas")
	if err != nil {
		t.Fatalf("resolve global: %v", err)
	}
	if global.Name != "global" || global.BaseURL != upstreamBaseGlobal || global.Origin != originRefererGlobal || global.DefaultDomain != "www.workbuddy.ai" {
		t.Fatalf("global config=%+v", global)
	}
	if !strings.Contains(filepath.Base(global.StateFile), "global") {
		t.Fatalf("global state file=%q", global.StateFile)
	}

	if _, err := resolveLoginRegion("all"); err == nil {
		t.Fatal("all must not select a single OAuth endpoint")
	}
}

func TestResolveLoginPortalRejectsUnknownValue(t *testing.T) {
	t.Setenv("WB2A_LOGIN_PORTAL", "other")
	if _, err := resolveLoginRegion("cn"); err == nil {
		t.Fatal("unknown portal should fail")
	}
}

func TestCommonHeadersUseRegion(t *testing.T) {
	cfg := loginRegion{Origin: "https://www.workbuddy.ai"}
	req := httptest.NewRequest(http.MethodGet, "https://example.test", nil)
	commonHeaders(req, cfg)
	if req.Header.Get("Origin") != cfg.Origin || req.Header.Get("Referer") != cfg.Origin+"/" {
		t.Fatalf("headers=%v", req.Header)
	}
}

func TestReadLoginStateChecksRegion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"state":"s1","region":"global"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := loginRegion{Name: "global", StateFile: path}
	state, gotPath, err := readLoginState(cfg)
	if err != nil || state.State != "s1" || gotPath != path {
		t.Fatalf("state=%+v path=%q err=%v", state, gotPath, err)
	}
	if _, _, err := readLoginState(loginRegion{Name: "cn", StateFile: path}); err == nil {
		t.Fatal("region mismatch should fail")
	}
}

func TestDoJSONUsesConfiguredEndpointAndHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/plugin/auth/state" || r.URL.Query().Get("platform") != "CLI" {
			t.Errorf("path=%s query=%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Origin") != "https://www.workbuddy.ai" {
			t.Errorf("origin=%q", r.Header.Get("Origin"))
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "{}" {
			t.Errorf("body=%q", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"state":"s","authUrl":"https://www.workbuddy.ai/login"}}`))
	}))
	defer srv.Close()
	cfg := loginRegion{BaseURL: srv.URL, Origin: "https://www.workbuddy.ai"}
	data, status, err := doJSON(http.DefaultClient, cfg, http.MethodPost, cfg.endpoint("/v2/plugin/auth/state?platform=CLI"), nil, strings.NewReader("{}"))
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if !strings.Contains(string(data), `"state":"s"`) {
		t.Fatalf("data=%s", data)
	}
}
