package upstream

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

type failingStreamReader struct{ err error }

func (r failingStreamReader) Read([]byte) (int, error) { return 0, r.err }

func TestStreamReadFailureEmitsErrorBeforeDone(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"timeout", context.DeadlineExceeded, "upstream_timeout"},
		{"connection_reset", io.ErrUnexpectedEOF, "upstream_stream_error"},
	} {
		for _, prefix := range []string{"", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"} {
			t.Run(tc.name+"/"+prefix, func(t *testing.T) {
				rec := httptest.NewRecorder()
				err := Stream(rec, io.MultiReader(strings.NewReader(prefix), failingStreamReader{tc.err}))
				if !errors.Is(err, tc.err) {
					t.Fatalf("err=%v want wrapped %v", err, tc.err)
				}
				body := rec.Body.String()
				if !strings.Contains(body, `"code":"`+tc.code+`"`) {
					t.Fatalf("missing error code: %s", body)
				}
				if strings.Count(body, "data: [DONE]") != 1 || !strings.HasSuffix(body, "data: [DONE]\n\n") {
					t.Fatalf("missing terminal DONE: %s", body)
				}
			})
		}
	}
}

func TestStreamUpstreamErrorStopsFurtherContent(t *testing.T) {
	raw := "data: {\"error\":{\"message\":\"quota exceeded\",\"type\":\"upstream_error\"}}\n\n" + sseFixture
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err == nil {
		t.Fatal("expected upstream error")
	}
	body := rec.Body.String()
	if strings.Count(body, `"error"`) != 1 || strings.Contains(body, "你好") {
		t.Fatalf("error duplicated or later content forwarded: %s", body)
	}
}
