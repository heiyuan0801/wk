package metricsstore

import (
	"testing"
)

func TestRecordRequestKeepsDuplicateExternalIDs(t *testing.T) {
	store, err := Open(t.TempDir() + "/metrics.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	base := RequestRecord{ID: "upstream-id", CreatedAt: 1, Route: "/v1/chat/completions", Model: "glm-5v-turbo", Mode: "stream", Status: 200}
	if err := store.RecordRequest(base); err != nil {
		t.Fatal(err)
	}
	base.CreatedAt = 2
	if err := store.RecordRequest(base); err != nil {
		t.Fatal(err)
	}
	base.ID = ""
	base.CreatedAt = 3
	if err := store.RecordRequest(base); err != nil {
		t.Fatal(err)
	}

	rows, err := store.RecentRequests(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d request rows, want 3: %#v", len(rows), rows)
	}
	if rows[0].ID != "" || rows[1].ID != "upstream-id" || rows[2].ID != "upstream-id" {
		t.Fatalf("unexpected order or IDs: %#v", rows)
	}
}
