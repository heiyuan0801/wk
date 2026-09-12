package smslogin

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// recordingProxy 是一个真的 HTTP 正向代理（CONNECT + 绝对 URI），
// 记录每个被代理的请求，用于证明登录链路确实全部经过代理。
type recordingProxy struct {
	server *httptest.Server
	mu     sync.Mutex
	uris   []string
	// auth 记录收到的 Proxy-Authorization（账密认证凭据）。
	auths []string
}

func newRecordingProxy(t *testing.T) *recordingProxy {
	t.Helper()
	p := &recordingProxy{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.uris = append(p.uris, r.Method+" "+hostOf(r))
		if a := r.Header.Get("Proxy-Authorization"); a != "" {
			p.auths = append(p.auths, a)
		}
		p.mu.Unlock()

		// CONNECT：直接挂断，隧道测试交给绝对 URI 即可。
		if r.Method == http.MethodConnect {
			http.Error(w, "no tunnel", http.StatusBadGateway)
			return
		}
		// 正向代理的绝对 URI 形式：转发到目标。
		out, err := http.NewRequestWithContext(r.Context(), r.Method, r.RequestURI, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out.Header = r.Header.Clone()
		resp, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	return p
}

func (p *recordingProxy) Close() { p.server.Close() }

func (p *recordingProxy) URL(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse(p.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (p *recordingProxy) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.uris)
}

func hostOf(r *http.Request) string {
	if r.Host != "" {
		return r.Host
	}
	u, err := url.Parse(r.RequestURI)
	if err != nil {
		return r.RequestURI
	}
	return u.Host
}

// TestFullLoginFlowThroughProxy 端到端证明：配置代理后，整条登录链路
// （OneID + Console/Keycloak + CLI）的每个请求都经代理发出。
func TestFullLoginFlowThroughProxy(t *testing.T) {
	proxy := newRecordingProxy(t)
	defer proxy.Close()

	f := newFakeUpstream()
	defer f.close()

	// 把四个 endpoint 都指向 fake 上游，但走代理转发。
	// 代理是 httptest 的 http://127.0.0.1:port，sid 会拼进用户名。
	d := NewResolverProxyDialer(
		"http://proxyuser:proxypass@"+proxy.URL(t).Host, 30)
	d.InjectSID = true
	d.randSID = func() string { return "e2e-sid" }

	m := NewManager(f.endpoints(), 0)
	m.SetProxyDialer(d)

	send, err := m.Send(context.Background(), "+852 64087495", "cn")
	if err != nil {
		t.Fatalf("send through proxy: %v", err)
	}
	if _, err := m.Verify(context.Background(), send.SessionID, "123456"); err != nil {
		t.Fatalf("verify through proxy: %v", err)
	}

	// 登录链路远不止一个请求；全部都要经代理。
	if got := proxy.count(); got < 10 {
		t.Fatalf("expected the full login chain (>=10 requests) via proxy, got %d", got)
	}
	// 账密认证：代理必须收到携带 sid 的凭据。凭据是 base64(user:pass)，
	// 必须解码后断言——base64 之后明文里看不到 sid。
	if len(proxy.auths) == 0 {
		t.Fatal("proxy must receive Proxy-Authorization for credential auth")
	}
	found := false
	for _, a := range proxy.auths {
		if decoded, err := base64.StdEncoding.DecodeString(
			strings.TrimPrefix(a, "Basic ")); err == nil && strings.Contains(string(decoded), "e2e-sid") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("proxy auth must carry the session id, got %v", proxy.auths[:min(3, len(proxy.auths))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
