// Package metricsstore persists aggregate request metrics in SQLite.
package metricsstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Snapshot is the persisted aggregate used by the HTTP status endpoints.
type Snapshot struct {
	Requests        int64 `json:"requests"`
	Successes       int64 `json:"successes"`
	Failures        int64 `json:"failures"`
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
	CacheRead       int64 `json:"cache_read_tokens"`
	CacheWrite      int64 `json:"cache_write_tokens"`
	ToolCalls       int64 `json:"tool_calls"`
	TTFBMillis      int64 `json:"ttfb_millis"`
	TTFBSamples     int64 `json:"ttfb_samples"`
	LatencyMillis   int64 `json:"latency_millis"`
	LastRequestUnix int64 `json:"last_request_at"`
}

// Store is safe for concurrent request completion writes.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS metrics (id INTEGER PRIMARY KEY CHECK (id = 1), data TEXT NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Add(delta Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.snapshotLocked()
	if err != nil {
		return err
	}
	current.Requests += delta.Requests
	current.Successes += delta.Successes
	current.Failures += delta.Failures
	current.InputTokens += delta.InputTokens
	current.OutputTokens += delta.OutputTokens
	current.TotalTokens += delta.TotalTokens
	current.CacheRead += delta.CacheRead
	current.CacheWrite += delta.CacheWrite
	current.ToolCalls += delta.ToolCalls
	current.TTFBMillis += delta.TTFBMillis
	current.TTFBSamples += delta.TTFBSamples
	current.LatencyMillis += delta.LatencyMillis
	if delta.LastRequestUnix > current.LastRequestUnix {
		current.LastRequestUnix = delta.LastRequestUnix
	}
	raw, err := json.Marshal(current)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO metrics(id, data, updated_at) VALUES(1, ?, ?) ON CONFLICT(id) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at`, string(raw), time.Now().Unix())
	return err
}

func (s *Store) Snapshot() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Store) snapshotLocked() (Snapshot, error) {
	var raw string
	err := s.db.QueryRow(`SELECT data FROM metrics WHERE id=1`).Scan(&raw)
	if err == sql.ErrNoRows {
		return Snapshot{}, nil
	}
	if err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode metrics: %w", err)
	}
	return snapshot, nil
}

func (s *Store) Close() error { return s.db.Close() }
