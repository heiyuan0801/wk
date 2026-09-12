package upstream

import (
	"net/http"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestOfficialClientHeadersFollowAccountDomain(t *testing.T) {
	for _, tc := range []struct {
		name, domain, origin string
	}{
		{name: "cn default", origin: "https://www.workbuddy.cn"},
		{name: "cn portal", domain: "www.codebuddy.cn", origin: "https://www.codebuddy.cn"},
		{name: "ai", domain: "www.workbuddy.ai", origin: "https://www.workbuddy.ai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &auth.Auth{Domain: tc.domain, AccessToken: "token", UID: "uid"}
			req, err := http.NewRequest(http.MethodPost, "https://upstream.invalid", nil)
			if err != nil {
				t.Fatal(err)
			}
			ChatHeaders(req, a)
			if got := req.Header.Get("User-Agent"); got != clientUA {
				t.Fatalf("user-agent=%q want %q", got, clientUA)
			}
			if got := req.Header.Get("X-Domain"); got != a.RequestDomain() {
				t.Fatalf("x-domain=%q want %q", got, a.RequestDomain())
			}
			if got := req.Header.Get("Origin"); got != tc.origin {
				t.Fatalf("origin=%q want %q", got, tc.origin)
			}
		})
	}
}

func TestBillingHeadersMatchElectronShape(t *testing.T) {
	a := &auth.Auth{Domain: "www.workbuddy.ai", AccessToken: "token", UID: "uid"}
	req, err := http.NewRequest(http.MethodPost, "https://upstream.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	BillingHeaders(req, a)
	for key, want := range map[string]string{
		"X-Requested-With": "XMLHttpRequest",
		"X-Product":        "SaaS",
		"X-Domain":         "www.workbuddy.ai",
		"Origin":           "https://www.workbuddy.ai",
	} {
		if got := req.Header.Get(key); got != want {
			t.Fatalf("%s=%q want %q", key, got, want)
		}
	}
}
