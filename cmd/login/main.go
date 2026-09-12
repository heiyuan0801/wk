// Command login implements the two-step WorkBuddy OAuth device flow used by
// the management console and login.sh.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

const (
	upstreamBaseCN      = "https://copilot.tencent.com"
	upstreamBaseGlobal  = "https://www.workbuddy.ai"
	originRefererCN     = "https://www.codebuddy.cn"
	originRefererWorkCN = "https://www.workbuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
	clientUA            = "WorkBuddy/5.5.2 CLI/2.137.1"
	legacyStateFile     = "/tmp/wb2api-login-state.json"
	maxResponseBytes    = 1 << 20
)

type loginRegion struct {
	Name          string
	BaseURL       string
	Origin        string
	DefaultDomain string
	Portal        string
	StateFile     string
}

func (r loginRegion) endpoint(path string) string {
	return strings.TrimRight(r.BaseURL, "/") + path
}

// resolveLoginRegion maps the user-facing region name to the OAuth host and
// browser origin. Empty means CN for backward compatibility. The management
// server passes WB2A_LOGIN_REGION; the standalone command also accepts an
// optional third argument: login <url|poll> [cn|global].
func resolveLoginRegion(raw string) (loginRegion, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("WB2A_LOGIN_REGION"))
		// WB2A_REGION may be present beside the server, but an account-pool
		// value of all/mixed cannot select one login endpoint.
		if raw == "" {
			envRegion := strings.TrimSpace(os.Getenv("WB2A_REGION"))
			if envRegion != "" && !strings.EqualFold(envRegion, "all") && !strings.EqualFold(envRegion, "mixed") {
				raw = envRegion
			}
		}
	}
	switch strings.ToLower(raw) {
	case "", "cn", "china":
		cfg := newLoginRegion("cn", upstreamBaseCN, originRefererCN, "")
		portal, err := resolveLoginPortal("")
		if err != nil {
			return loginRegion{}, err
		}
		cfg.Portal = portal
		cfg.Origin = loginPortalOrigin(portal)
		return cfg, nil
	case "global", "overseas", "international", "intl":
		cfg := newLoginRegion("global", upstreamBaseGlobal, originRefererGlobal, "www.workbuddy.ai")
		cfg.Portal = "global"
		return cfg, nil
	case "all", "mixed":
		return loginRegion{}, fmt.Errorf("login region must be cn or global, not %q", raw)
	default:
		return loginRegion{}, fmt.Errorf("unknown login region %q (want cn or global)", raw)
	}
}

func resolveLoginPortal(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("WB2A_LOGIN_PORTAL"))
	}
	switch strings.ToLower(raw) {
	case "", "codebuddy", "code-buddy", "codebuddy.cn":
		return "codebuddy", nil
	case "workbuddy", "work-buddy", "workbuddy.cn":
		return "workbuddy", nil
	default:
		return "", fmt.Errorf("login portal must be codebuddy or workbuddy, not %q", raw)
	}
}

func loginPortalOrigin(portal string) string {
	if portal == "workbuddy" {
		return originRefererWorkCN
	}
	return originRefererCN
}

func newLoginRegion(name, baseURL, origin, defaultDomain string) loginRegion {
	stateFile := strings.TrimSpace(os.Getenv("WB2A_LOGIN_STATE_FILE"))
	if stateFile == "" {
		stateFile = filepath.Join(os.TempDir(), "wb2api-login-state-"+name+".json")
	}
	return loginRegion{
		Name:          name,
		BaseURL:       baseURL,
		Origin:        origin,
		DefaultDomain: defaultDomain,
		StateFile:     stateFile,
	}
}

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type loginState struct {
	State  string `json:"state"`
	Region string `json:"region,omitempty"`
}

func main() {
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fatal("usage: login <url|poll> [cn|global]")
	}
	regionArg := ""
	if len(os.Args) == 3 {
		regionArg = os.Args[2]
	}
	cfg, err := resolveLoginRegion(regionArg)
	if err != nil {
		fatal("%v", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		fatal("create cookie jar: %v", err)
	}
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}
	switch os.Args[1] {
	case "url":
		startLogin(client, cfg)
	case "poll":
		pollLogin(client, cfg)
	default:
		fatal("unknown subcommand %q (want url|poll)", os.Args[1])
	}
}

func startLogin(client *http.Client, cfg loginRegion) {
	data, _, err := doJSON(client, cfg, http.MethodPost, cfg.endpoint("/v2/plugin/auth/state?platform=CLI"), nil, bytes.NewReader([]byte("{}")))
	if err != nil {
		fatal("auth state failed: %v", err)
	}
	var state struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &state); err != nil || state.State == "" || state.AuthURL == "" {
		fatal("auth state response is missing state or authUrl")
	}
	state.AuthURL = rewriteLoginURL(state.AuthURL, cfg.Portal)
	raw, err := json.Marshal(loginState{State: state.State, Region: cfg.Name})
	if err != nil {
		fatal("encode state: %v", err)
	}
	if dir := filepath.Dir(cfg.StateFile); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fatal("create state directory: %v", err)
		}
	}
	if err := os.WriteFile(cfg.StateFile, raw, 0o600); err != nil {
		fatal("write state: %v", err)
	}
	_ = os.Chmod(cfg.StateFile, 0o600)
	fmt.Println(state.AuthURL)
}

func rewriteLoginURL(raw, portal string) string {
	if portal != "codebuddy" && portal != "workbuddy" {
		return raw
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return raw
	}
	parsed.Scheme = "https"
	if portal == "workbuddy" {
		parsed.Host = "www.workbuddy.cn"
	} else {
		parsed.Host = "www.codebuddy.cn"
	}
	return parsed.String()
}

func pollLogin(client *http.Client, cfg loginRegion) {
	state, statePath, err := readLoginState(cfg)
	if err != nil {
		fatal("%v (run login url first)", err)
	}
	escapedState := url.QueryEscape(state.State)
	tokenRaw, status, err := doJSON(client, cfg, http.MethodGet, cfg.endpoint("/v2/plugin/auth/token?state=")+escapedState, nil, nil)
	if err != nil {
		if status == 0 || status >= 500 {
			fatal("token endpoint error: %v", err)
		}
		fatal("login is not complete; finish authorization in the browser and retry")
	}
	var token struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokenRaw, &token); err != nil || token.AccessToken == "" {
		fatal("login is not complete; finish authorization in the browser and retry")
	}

	var account struct {
		UID          string `json:"uid"`
		UserID       string `json:"userId"`
		ID           string `json:"id"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
		Name         string `json:"name"`
		Account      struct {
			UID          string `json:"uid"`
			UserID       string `json:"userId"`
			ID           string `json:"id"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			Name         string `json:"name"`
		} `json:"account"`
	}
	headers := func(req *http.Request) {
		commonHeaders(req, cfg)
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	}
	accountRaw, _, accountErr := doJSON(client, cfg, http.MethodGet, cfg.endpoint("/v2/plugin/login/account?state=")+escapedState, headers, nil)
	if accountErr != nil {
		fatal("account lookup failed: %v", accountErr)
	}
	if err := json.Unmarshal(accountRaw, &account); err != nil {
		fatal("account response is invalid: %v", err)
	}
	uid := firstNonEmpty(account.UID, account.UserID, account.ID, account.Account.UID, account.Account.UserID, account.Account.ID)
	enterpriseID := firstNonEmpty(account.EnterpriseID, account.Account.EnterpriseID)
	nickname := firstNonEmpty(account.Nickname, account.Name, account.Account.Nickname, account.Account.Name)
	if uid == "" {
		fatal("account response is missing uid")
	}

	domain := strings.TrimSpace(token.Domain)
	if cfg.Name == "global" && (&auth.Auth{Domain: domain}).Region() != auth.RegionGlobal {
		// Keep the saved credential tied to the selected international host even
		// when the upstream omits domain or returns a legacy alias.
		domain = cfg.DefaultDomain
	} else if domain == "" {
		domain = cfg.DefaultDomain
	}

	result := map[string]any{
		"access_token": token.AccessToken, "refresh_token": token.RefreshToken,
		"expires_in": token.ExpiresIn, "domain": domain, "region": cfg.Name,
		"uid": uid, "enterprise_id": enterpriseID, "nickname": nickname,
	}
	out, err := json.Marshal(result)
	if err != nil {
		fatal("encode result: %v", err)
	}
	fmt.Println(string(out))
	_ = os.Remove(statePath)
}

func readLoginState(cfg loginRegion) (loginState, string, error) {
	paths := []string{cfg.StateFile}
	// Migrate the old single CN state file so an authorization started before
	// an upgrade can still be completed.
	if cfg.Name == "cn" && os.Getenv("WB2A_LOGIN_STATE_FILE") == "" {
		paths = append(paths, legacyStateFile)
	}
	var lastErr error
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}
		var state loginState
		if err := json.Unmarshal(raw, &state); err != nil || strings.TrimSpace(state.State) == "" {
			lastErr = fmt.Errorf("parse state: invalid state file")
			continue
		}
		if state.Region != "" && !strings.EqualFold(state.Region, cfg.Name) {
			return loginState{}, "", fmt.Errorf("state belongs to %s login, current request is %s", state.Region, cfg.Name)
		}
		return state, path, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("state file not found")
	}
	return loginState{}, "", lastErr
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func doJSON(client *http.Client, cfg loginRegion, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers == nil {
		commonHeaders(req, cfg)
	} else {
		headers(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		return nil, resp.StatusCode, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	var envelope apiEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse response: %w", err)
	}
	if envelope.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", envelope.Code, envelope.Msg)
	}
	return envelope.Data, resp.StatusCode, nil
}

func commonHeaders(req *http.Request, cfg loginRegion) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", cfg.Origin)
	req.Header.Set("Referer", strings.TrimRight(cfg.Origin, "/")+"/")
	req.Header.Set("User-Agent", clientUA)
}

func fatal(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}
