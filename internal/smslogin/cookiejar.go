package smslogin

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
)

// newConsoleJar 包装标准 cookiejar：Keycloak 的 AUTH_SESSION_ID 用 RFC 2109
// （Version=1，Path 带引号）。Go 的 net/http 解析后 Path 变成空，jar 会按请求
// URL 默认成 /auth/realms/copilot/protocol/openid-connect/，于是后续
// /login-actions/first-broker-login 带不上会话 Cookie，页面 403 并吐出
// checkCookiesAndSetTimeout。
func newConsoleJar() (http.CookieJar, error) {
	inner, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &realmPathJar{inner: inner}, nil
}

type realmPathJar struct {
	inner http.CookieJar
}

func (j *realmPathJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	fixed := make([]*http.Cookie, 0, len(cookies))
	for _, c := range cookies {
		cc := *c
		fixQuotedCookiePath(u, &cc)
		fixed = append(fixed, &cc)
	}
	j.inner.SetCookies(u, fixed)
}

func (j *realmPathJar) Cookies(u *url.URL) []*http.Cookie {
	return j.inner.Cookies(u)
}

func fixQuotedCookiePath(u *url.URL, c *http.Cookie) {
	c.Path = strings.Trim(c.Path, `"'`)
	if strings.HasPrefix(c.Path, "/") {
		return
	}
	if u == nil {
		return
	}
	// 只修补 Keycloak realm 下的会话 Cookie，避免把无关 Cookie 扩到整站。
	if realm := keycloakRealmPath(u.Path); realm != "" {
		c.Path = realm
	}
}

func keycloakRealmPath(p string) string {
	const prefix = "/auth/realms/"
	if !strings.HasPrefix(p, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(p, prefix)
	realm, _, _ := strings.Cut(rest, "/")
	if realm == "" {
		return ""
	}
	return prefix + realm
}

// cookieFixTransport 在 Go 解析 Set-Cookie 之前，把 Keycloak 的 RFC2109 头
// 改写成 net/http 能认的形态。只包 jar 不够：有些组合（Version=1 + 引号 Path
// + SameSite）会让 Cookies() 直接丢掉整颗 Cookie，jar.SetCookies 根本看不到。
type cookieFixTransport struct {
	base http.RoundTripper
}

func unwrapTransport(rt http.RoundTripper) http.RoundTripper {
	for {
		fix, ok := rt.(*cookieFixTransport)
		if !ok {
			return rt
		}
		rt = fix.base
	}
}

func wrapCookieFix(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if _, ok := base.(*cookieFixTransport); ok {
		return base
	}
	return &cookieFixTransport{base: base}
}

func (t *cookieFixTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	lines := resp.Header.Values("Set-Cookie")
	if len(lines) == 0 {
		return resp, nil
	}
	resp.Header.Del("Set-Cookie")
	for _, line := range lines {
		resp.Header.Add("Set-Cookie", rewriteKeycloakSetCookie(line, req.URL))
	}
	return resp, nil
}

var (
	versionCookieRe = regexp.MustCompile(`(?i);\s*Version\s*=\s*[^;]*`)
	quotedPathRe    = regexp.MustCompile(`(?i);\s*Path\s*=\s*"([^"]*)"`)
	pathPresentRe   = regexp.MustCompile(`(?i);\s*Path\s*=`)
)

func rewriteKeycloakSetCookie(line string, u *url.URL) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return line
	}
	line = quotedPathRe.ReplaceAllString(line, "; Path=$1")
	line = versionCookieRe.ReplaceAllString(line, "")
	if u != nil && !pathPresentRe.MatchString(";"+line) {
		if realm := keycloakRealmPath(u.Path); realm != "" {
			line += "; Path=" + realm
		}
	}
	line = strings.ReplaceAll(line, ";;", ";")
	return strings.TrimSpace(strings.TrimSuffix(line, ";"))
}

func cookieHeaderNames(header string) string {
	var names []string
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, _, _ := strings.Cut(part, "=")
		if name != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, ",")
}

func forceHTTPSConsole(u *url.URL) {
	if u == nil {
		return
	}
	host := strings.ToLower(u.Hostname())
	if host != consoleDomain && host != "codebuddy.cn" {
		return
	}
	if u.Scheme == "http" {
		u.Scheme = "https"
	}
	if u.Host == consoleDomain+":80" {
		u.Host = consoleDomain
	}
}

func cookieNamesOn(jar http.CookieJar, rawURL string) string {
	if jar == nil {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	var names []string
	for _, c := range jar.Cookies(u) {
		if c.Name != "" {
			names = append(names, c.Name)
		}
	}
	return strings.Join(names, ",")
}

// navigationReferer 用作下一跳 Referer。WAF 常把带 code= 的 Referer 直接 403，
// 所以清掉一次性授权参数，保留路径（Keycloak 只认来源 path）。
func navigationReferer(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return raw
	}
	q := u.Query()
	changed := false
	for _, k := range []string{"code", "session_state", "iss"} {
		if q.Has(k) {
			q.Del(k)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	u.RawQuery = q.Encode()
	return u.String()
}
