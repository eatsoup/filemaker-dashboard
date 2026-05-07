package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openSession(t *testing.T, b *IngestBatch, ts time.Time, cid, db, acct string) {
	t.Helper()
	if _, err := b.OpenSession(Session{
		StartTime: ts, ClientName: cid, ClientID: cid, Account: acct, Database: db,
	}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
}

// A duplicate "opening database" for the same (client_id, db, account) while
// a prior session is still in-flight is FileMaker's only signal that the old
// session was silently dropped (network blip, server hang/restart). The new
// open should auto-close the orphan instead of leaving it dangling forever.
func TestOpenSession_AutoClosesPriorInFlight(t *testing.T) {
	s := openTestStore(t)
	b, err := s.BeginIngest()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	t1 := time.Date(2026, 5, 4, 14, 36, 45, 0, time.UTC)
	t2 := t1.Add(3 * time.Minute)
	cid, db, acct := `client (HOST-A) [10.0.0.1]`, "Sales", "alice"

	openSession(t, b, t1, cid, db, acct)
	openSession(t, b, t2, cid, db, acct) // no DBClose between — should auto-close t1

	if err := b.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	rows, err := s.DB().Query(
		`SELECT start_time, end_time, duration_seconds FROM sessions
         WHERE client_id = ? AND database_name = ? AND account_name = ?
         ORDER BY start_time`,
		cid, db, acct,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	type row struct {
		start, end *int64
		dur        *int64
	}
	var got []row
	for rows.Next() {
		var st int64
		var en, du *int64
		if err := rows.Scan(&st, &en, &du); err != nil {
			t.Fatalf("scan: %v", err)
		}
		s := st
		got = append(got, row{start: &s, end: en, dur: du})
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	// First session must be closed at t2 with duration t2-t1.
	if got[0].end == nil || *got[0].end != t2.Unix() {
		t.Fatalf("first row end_time: got %v want %d", got[0].end, t2.Unix())
	}
	if got[0].dur == nil || *got[0].dur != int64((t2.Sub(t1)).Seconds()) {
		t.Fatalf("first row duration: got %v want %d", got[0].dur, int64(t2.Sub(t1).Seconds()))
	}
	// Second session is the new in-flight row.
	if got[1].end != nil {
		t.Fatalf("second row should be in-flight, got end_time=%v", *got[1].end)
	}

	// A subsequent CloseSession should close the second row (not the already-closed first).
	b2, _ := s.BeginIngest()
	closed, err := b2.CloseSession(cid, db, acct, t2.Add(5*time.Minute))
	if err != nil || !closed {
		t.Fatalf("CloseSession: closed=%v err=%v", closed, err)
	}
	if err := b2.Commit(); err != nil {
		t.Fatalf("commit2: %v", err)
	}

	var inflight int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sessions WHERE end_time IS NULL`).Scan(&inflight); err != nil {
		t.Fatalf("count: %v", err)
	}
	if inflight != 0 {
		t.Fatalf("want 0 in-flight rows after final close, got %d", inflight)
	}
}

// Re-processing the same "opening database" line (same start_time + triple)
// must remain a no-op: no new row, and no spurious auto-close of itself.
func TestOpenSession_IdempotentReplay(t *testing.T) {
	s := openTestStore(t)
	b, err := s.BeginIngest()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	ts := time.Date(2026, 5, 4, 14, 0, 0, 0, time.UTC)
	cid, db, acct := "ipad", "Sales", "alice"
	openSession(t, b, ts, cid, db, acct)
	openSession(t, b, ts, cid, db, acct) // identical replay
	if err := b.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var n, inflight int
	if err := s.DB().QueryRow(`SELECT COUNT(*), COUNT(*) FILTER (WHERE end_time IS NULL) FROM sessions`).Scan(&n, &inflight); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 || inflight != 1 {
		t.Fatalf("want 1 row in-flight after replay, got %d total / %d in-flight", n, inflight)
	}
}

// CloseAbandonedSessions is the safety net for sessions whose client never
// reconnects (so the auto-close-on-duplicate-open path never fires). Any
// in-flight row older than the threshold gets a synthetic close at
// start_time + threshold.
func TestCloseAbandonedSessions(t *testing.T) {
	s := openTestStore(t)
	b, _ := s.BeginIngest()

	now := time.Date(2026, 5, 7, 23, 30, 0, 0, time.UTC)
	old := now.Add(-15 * time.Hour)    // beyond 12h threshold
	recent := now.Add(-30 * time.Minute) // within threshold
	openSession(t, b, old, "abandoned-client", "Sales", "x")
	openSession(t, b, recent, "live-client", "Sales", "y")
	if err := b.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	reaped, err := s.CloseAbandonedSessions(now, 12*time.Hour)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("want 1 reaped, got %d", reaped)
	}

	var endX, durX *int64
	if err := s.DB().QueryRow(
		`SELECT end_time, duration_seconds FROM sessions WHERE client_id = ?`, "abandoned-client",
	).Scan(&endX, &durX); err != nil {
		t.Fatalf("query abandoned: %v", err)
	}
	wantEnd := old.Unix() + int64((12 * time.Hour).Seconds())
	if endX == nil || *endX != wantEnd {
		t.Fatalf("abandoned end_time: got %v want %d", endX, wantEnd)
	}
	if durX == nil || *durX != int64((12 * time.Hour).Seconds()) {
		t.Fatalf("abandoned duration: got %v want %d", durX, int64((12 * time.Hour).Seconds()))
	}

	var endY *int64
	if err := s.DB().QueryRow(
		`SELECT end_time FROM sessions WHERE client_id = ?`, "live-client",
	).Scan(&endY); err != nil {
		t.Fatalf("query live: %v", err)
	}
	if endY != nil {
		t.Fatalf("recent session should still be in-flight, got end_time=%d", *endY)
	}
}
