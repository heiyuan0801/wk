package main

import (
	"path/filepath"
	"testing"

	"workbuddy2api/internal/metricsstore"
	"workbuddy2api/internal/server"
)

func TestCombinedMetricsAdapterPreservesUsageAndCreditFields(t *testing.T) {
	s, err := metricsstore.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := metricsAdapter{store: s}
	for _, code := range []int{200, 400} {
		err := m.RecordCompletion(server.RequestLog{ID: "WB/unchanged", Status: code, InputTokens: 10, OutputTokens: 4, TotalTokens: 14,
			CacheReadTokens: 3, CacheWriteTokens: 2, ToolCalls: 1, LatencyMillis: 100, TTFBMillis: 0, CreditsConsumed: .25, CreditSource: "estimated"}, true)
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 2 || got.Successes != 1 || got.Failures != 1 || got.InputTokens != 20 || got.OutputTokens != 8 || got.TotalTokens != 28 ||
		got.CacheRead != 6 || got.CacheWrite != 4 || got.ToolCalls != 2 || got.LatencyMillis != 200 || got.TTFBSamples != 2 ||
		got.CreditsEstimated != .5 || got.CreditsConsumed != .5 || got.CreditRequests != 2 {
		t.Fatalf("metrics=%+v", got)
	}
	logs, err := m.RecentRequests(10)
	if err != nil || len(logs) != 2 || logs[0].ID != "WB/unchanged" || logs[0].Status != 400 {
		t.Fatalf("logs=%+v err=%v", logs, err)
	}
}
