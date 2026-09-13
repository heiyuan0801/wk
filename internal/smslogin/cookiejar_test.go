package smslogin

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestQuotedKeycloakCookieReachesFirstBrokerLogin(t *testing.T) {
	var gotCookie string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/realms/copilot/protocol/openid-connect/auth":
			w.Header().Add("Set-Cookie", `AUTH_SESSION_ID=sess-value; Version=1; Path="/auth/realms/copilot"; HttpOnly`)
			w.WriteHeader(200)
		case "/auth/realms/copilot/login-actions/first-broker-login":
			gotCookie = r.Header.Get("Cookie")
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	}))
	defer ts.Close()

	jar, err := newConsoleJar()
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Transport: wrapCookieFix(nil)}

	authURL := ts.URL + "/auth/realms/copilot/protocol/openid-connect/auth"
	resp, err := client.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	loginURL := ts.URL + "/auth/realms/copilot/login-actions/first-broker-login?client_id=console"
	resp, err = client.Get(loginURL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if !strings.Contains(gotCookie, "AUTH_SESSION_ID=sess-value") {
		t.Fatalf("first-broker-login missing AUTH_SESSION_ID: %q", gotCookie)
	}
}

func TestStdJarDropsQuotedPathCookie(t *testing.T) {
	var gotCookie string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/realms/copilot/protocol/openid-connect/auth":
			w.Header().Add("Set-Cookie", `AUTH_SESSION_ID=sess-value; Version=1; Path="/auth/realms/copilot"; HttpOnly`)
			w.WriteHeader(200)
		case "/auth/realms/copilot/login-actions/first-broker-login":
			gotCookie = r.Header.Get("Cookie")
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	}))
	defer ts.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	if _, err := client.Get(ts.URL + "/auth/realms/copilot/protocol/openid-connect/auth"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(ts.URL + "/auth/realms/copilot/login-actions/first-broker-login"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotCookie, "AUTH_SESSION_ID") {
		t.Fatalf("expected std jar to drop quoted-path cookie, got %q", gotCookie)
	}
}

func TestRewriteKeycloakSetCookieUnquotesPath(t *testing.T) {
	u, _ := url.Parse("https://www.codebuddy.cn/auth/realms/copilot/protocol/openid-connect/auth")
	got := rewriteKeycloakSetCookie(`AUTH_SESSION_ID=sess-value; Version=1; Path="/auth/realms/copilot"; HttpOnly`, u)
	if strings.Contains(got, "Version=") {
		t.Fatalf("Version must be stripped: %s", got)
	}
	if strings.Contains(got, `Path="/auth`) {
		t.Fatalf("quoted Path must be unquoted: %s", got)
	}
	if !strings.Contains(got, `Path=/auth/realms/copilot`) {
		t.Fatalf("Path missing: %s", got)
	}
}

func TestForceHTTPSConsole(t *testing.T) {
	u, _ := url.Parse("http://www.codebuddy.cn/auth/realms/copilot/login-actions/first-broker-login?session_code=x")
	forceHTTPSConsole(u)
	if u.Scheme != "https" {
		t.Fatalf("scheme=%s", u.Scheme)
	}
}

func TestNavigationRefererStripsAuthCode(t *testing.T) {
	got := navigationReferer("https://www.codebuddy.cn/auth/realms/copilot/broker/oneid/endpoint?code=abc&state=s1")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("code") != "" {
		t.Fatalf("code must be stripped: %s", got)
	}
	if u.Query().Get("state") != "s1" {
		t.Fatalf("state should remain: %s", got)
	}
}
