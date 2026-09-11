package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

func TestResponsesPreservesPromptCacheFields(t *testing.T) {
	body, stream, err := responsesToChat([]byte(`{
		"model":"glm-5.2",
		"input":"hello",
		"instructions":"be concise",
		"stream":true,
		"prompt_cache_key":"agent-42",
		"prompt_cache_retention":"24h"
	}`))
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	if !stream {
		t.Fatal("stream=false")
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("chat body is not JSON: %v", err)
	}
	if got["prompt_cache_key"] != "agent-42" || got["prompt_cache_retention"] != "24h" {
		t.Fatalf("cache fields lost: %v", got)
	}
	if got["input"] != nil || got["instructions"] != nil {
		t.Fatalf("Responses-only input fields leaked into chat body: %v", got)
	}
}

func TestResponsesInputImageConvertsToChatImageURL(t *testing.T) {
	body, _, err := responsesToChat([]byte(`{
		"model":"glm-5.2",
		"input":[{"role":"user","content":[
			{"type":"input_text","text":"describe this"},
			{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"high"}
		]}]
	}`))
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("chat body is not JSON: %v", err)
	}
	messages := got["messages"].([]any)
	content := messages[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content parts=%d, want 2: %#v", len(content), content)
	}
	image := content[1].(map[string]any)
	if image["type"] != "image_url" {
		t.Fatalf("converted image type=%v", image["type"])
	}
	imageURL := image["image_url"].(map[string]any)
	if imageURL["url"] != "data:image/png;base64,AA==" || imageURL["detail"] != "high" {
		t.Fatalf("converted image_url=%#v", imageURL)
	}
}

func TestResponsesMalformedInputImageIsRejected(t *testing.T) {
	body, _, err := responsesToChat([]byte(`{
		"model":"glm-5.2",
		"input":[{"role":"user","content":[{"type":"input_image","image_url":{}}]}]
	}`))
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	if err := validateImageParts(body); err == nil {
		t.Fatalf("malformed input_image should be rejected after conversion; body=%s", body)
	}
}

func TestResponsesPromptCacheKeyKeepsAccountAffinity(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		mu.Lock()
		auths = append(auths, authz)
		mu.Unlock()
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	sess := session.New(session.Config{Available: p.AvailableUIDs})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})

	for i := 0; i < 2; i++ {
		rec := doResponsesRequest(h, `{"model":"glm-5.2","input":"hello","stream":true,"prompt_cache_key":"agent-42"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: code=%d body=%s", i, rec.Code, rec.Body)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 2 || auths[0] == "" || auths[0] != auths[1] {
		t.Fatalf("same prompt cache key should keep account affinity: %v", auths)
	}
}

func TestResponsesPreviousResponseIDIsNotUsedAsUnstableSessionKey(t *testing.T) {
	key := `{"previous_response_id":"resp-123"}`
	if got := session.ExtractKey([]byte(key)); got != "" {
		t.Fatalf("ExtractKey(previous_response_id)=%q", got)
	}
}

func TestResponsesCacheKeyPriorityPreservesConversationAffinity(t *testing.T) {
	body := []byte(`{"conversation_id":"conversation-1","prompt_cache_key":"cache-1"}`)
	if got := session.ExtractKey(body); got != "conversation-1" {
		t.Fatalf("conversation affinity should remain higher priority, got %q", got)
	}
	if got := session.ExtractKey([]byte(`{"conversation":"conversation-2","prompt_cache_key":"cache-2"}`)); got != "conversation-2" {
		t.Fatalf("Responses conversation should keep affinity, got %q", got)
	}
}

func TestResponsesNonStreamPreservesWorkBuddyRequestID(t *testing.T) {
	const upstreamID = "WB-Request.Mixed_123/abc"
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"request_id\":\"" + upstreamID + "\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponsesRequest(h, `{"model":"glm-5.2","input":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["id"] != upstreamID {
		t.Fatalf("response ID=%q want unchanged %q", response["id"], upstreamID)
	}
	if rec.Header().Get("X-Request-Id") != upstreamID {
		t.Fatalf("response header ID=%q", rec.Header().Get("X-Request-Id"))
	}
	if _, ok := h.responseHistory[upstreamID]; !ok {
		t.Fatalf("previous_response_id history was not keyed by upstream ID: %#v", h.responseHistory)
	}
}

func TestResponsesStreamPreservesWorkBuddyRecordID(t *testing.T) {
	const upstreamID = "WB.Record-ID_456"
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"record_id\":\"" + upstreamID + "\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := doResponsesRequest(h, `{"model":"glm-5.2","input":"hello","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"id":"`+upstreamID+`"`) ||
		!strings.Contains(rec.Body.String(), `"response_id":"`+upstreamID+`"`) {
		t.Fatalf("stream replaced upstream ID: %s", rec.Body.String())
	}
	if rec.Header().Get("X-Request-Id") != upstreamID {
		t.Fatalf("response header ID=%q", rec.Header().Get("X-Request-Id"))
	}
	if _, ok := h.responseHistory[upstreamID]; !ok {
		t.Fatalf("stream history was not keyed by upstream ID: %#v", h.responseHistory)
	}
}

func doResponsesRequest(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	return rec
}
