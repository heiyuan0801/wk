package metricsstore

import (
	"strconv"
	"testing"
	"time"
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

func TestDeleteRequestLogsBeforeKeepsAggregates(t *testing.T) {
	store, err := Open(t.TempDir() + "/metrics.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, created := range []int64{100, 200, 300} {
		if err := store.RecordCompletion(Snapshot{Requests: 1, Successes: 1}, RequestRecord{ID: "id-" + strconv.FormatInt(created, 10), CreatedAt: created, Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := store.DeleteRequestLogsBefore(time.Unix(200, 0))
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	rows, err := store.RecentRequests(10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	snapshot, err := store.Snapshot()
	if err != nil || snapshot.Requests != 3 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
}
