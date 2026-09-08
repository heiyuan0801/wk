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
	"time"
)

const (
	upstreamBaseCN    = "https://copilot.tencent.com"
	originReferer     = "https://www.codebuddy.cn"
	clientUA          = "CLI/2.63.2 CodeBuddy/2.63.2"
	endpointAuthState = upstreamBaseCN + "/v2/plugin/auth/state?platform=CLI"
	endpointAuthToken = upstreamBaseCN + "/v2/plugin/auth/token?state="
	endpointAccount   = upstreamBaseCN + "/v2/plugin/login/account?state="
	stateFile         = "/tmp/wb2api-login-state.json"
	maxResponseBytes  = 1 << 20
)

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type loginState struct {
	State string `json:"state"`
}

func main() {
	if len(os.Args) != 2 {
		fatal("usage: login <url|poll>")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		fatal("create cookie jar: %v", err)
	}
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}
	switch os.Args[1] {
	case "url":
		startLogin(client)
	case "poll":
		pollLogin(client)
	default:
		fatal("unknown subcommand %q (want url|poll)", os.Args[1])
	}
}

func startLogin(client *http.Client) {
	data, _, err := doJSON(client, http.MethodPost, endpointAuthState, nil, bytes.NewReader([]byte("{}")))
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
	raw, err := json.Marshal(loginState{State: state.State})
	if err != nil {
		fatal("encode state: %v", err)
	}
	if err := os.WriteFile(stateFile, raw, 0o600); err != nil {
		fatal("write state: %v", err)
	}
	fmt.Println(state.AuthURL)
}

func pollLogin(client *http.Client) {
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		fatal("read state: %v (run login url first)", err)
	}
	var state loginState
	if err := json.Unmarshal(raw, &state); err != nil || state.State == "" {
		fatal("parse state: invalid state file")
	}
	escapedState := url.QueryEscape(state.State)
	tokenRaw, status, err := doJSON(client, http.MethodGet, endpointAuthToken+escapedState, nil, nil)
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
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	headers := func(req *http.Request) {
		commonHeaders(req)
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	}
	accountRaw, _, accountErr := doJSON(client, http.MethodGet, endpointAccount+escapedState, headers, nil)
	if accountErr != nil {
		fatal("account lookup failed: %v", accountErr)
	}
	if err := json.Unmarshal(accountRaw, &account); err != nil || account.UID == "" {
		fatal("account response is missing uid")
	}

	result := map[string]any{
		"access_token": token.AccessToken, "refresh_token": token.RefreshToken,
		"expires_in": token.ExpiresIn, "domain": token.Domain,
		"uid": account.UID, "enterprise_id": account.EnterpriseID, "nickname": account.Nickname,
	}
	out, err := json.Marshal(result)
	if err != nil {
		fatal("encode result: %v", err)
	}
	fmt.Println(string(out))
	_ = os.Remove(stateFile)
}

func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers == nil {
		commonHeaders(req)
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

func commonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", originReferer)
	req.Header.Set("Referer", originReferer+"/")
	req.Header.Set("User-Agent", clientUA)
}

func fatal(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}
