package upstream

import (
	"net/http"
	"testing"
)

func TestResponseIDSupportsWorkBuddyFieldNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"openai id", map[string]any{"id": "chatcmpl-WB"}, "chatcmpl-WB"},
		{"snake request id", map[string]any{"request_id": "Req_ABC"}, "Req_ABC"},
		{"camel request id", map[string]any{"requestId": "Req/123"}, "Req/123"},
		{"snake record id", map[string]any{"record_id": "Record-9"}, "Record-9"},
		{"camel record id", map[string]any{"recordId": "Record.Mixed_Case"}, "Record.Mixed_Case"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResponseID(tc.body); got != tc.want {
				t.Fatalf("ResponseID()=%q want %q", got, tc.want)
			}
		})
	}
}

func TestResponseIDHeadersAreCopiedVerbatim(t *testing.T) {
	src := http.Header{
		"X-Request-Id":       []string{"WB-Req_ABC/123"},
		"X-Request-Trace-Id": []string{"Trace.Mixed-Case"},
		"Set-Cookie":         []string{"private=1"},
	}
	dst := make(http.Header)
	CopyResponseIDHeaders(dst, src)
	if got := ResponseIDFromHeader(dst); got != "WB-Req_ABC/123" {
		t.Fatalf("request ID=%q", got)
	}
	if dst.Get("X-Request-Trace-Id") != "Trace.Mixed-Case" {
		t.Fatalf("trace ID=%q", dst.Get("X-Request-Trace-Id"))
	}
	if dst.Get("Set-Cookie") != "" {
		t.Fatal("unrelated upstream header leaked")
	}
}
