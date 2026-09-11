package metricsstore

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestCompletionsPersistConcurrentCountsAndRepeatedIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.RecordCompletion(Snapshot{Requests: 1, Successes: 1, CreditsConsumed: .5, CreditsUpstream: .5, CreditRequests: 1}, RequestRecord{ID: "upstream-same", Status: 200})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap, err := s.Snapshot()
	if err != nil || snap.Requests != 100 || snap.Successes != 100 || snap.CreditsConsumed != 50 || snap.CreditRequests != 100 {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
	rows, err := s.RecentRequests(200)
	if err != nil || len(rows) != 100 {
		t.Fatalf("records=%d err=%v", len(rows), err)
	}
	for _, row := range rows {
		if row.ID != "upstream-same" {
			t.Fatalf("rewritten ID: %q", row.ID)
		}
	}
}

func TestCompletionRollsBackMetricsIfLogInsertFails(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`CREATE TRIGGER fail_log BEFORE INSERT ON request_logs BEGIN SELECT RAISE(ABORT, 'test failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCompletion(Snapshot{Requests: 1}, RequestRecord{ID: "x"}); err == nil {
		t.Fatal("expected rollback")
	}
	snap, err := s.Snapshot()
	if err != nil || snap.Requests != 0 {
		t.Fatalf("partial metrics: %+v err=%v", snap, err)
	}
	if s.requestWrites != 0 {
		t.Fatal("failed transaction advanced retention counter")
	}
}
