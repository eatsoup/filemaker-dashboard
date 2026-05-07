package ingest

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"filemaker-dashboard/internal/parser"
	"filemaker-dashboard/internal/store"
)

type Ingester struct {
	store    *store.Store
	logfile  string
	interval time.Duration
	logger   *slog.Logger

	mu      sync.Mutex
	running bool
}

func New(s *store.Store, logfile string, interval time.Duration, logger *slog.Logger) *Ingester {
	if logger == nil {
		logger = slog.Default()
	}
	return &Ingester{store: s, logfile: logfile, interval: interval, logger: logger}
}

// Run blocks until ctx is cancelled. It runs an initial pass immediately,
// then ticks at the configured interval.
func (i *Ingester) Run(ctx context.Context) {
	if err := i.RunOnce(ctx); err != nil {
		i.logger.Error("initial ingest failed", "err", err)
	}
	t := time.NewTicker(i.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := i.RunOnce(ctx); err != nil {
				i.logger.Error("ingest failed", "err", err)
			}
		}
	}
}

// RunOnce processes new content in the configured logfile, advancing the
// (timestamp, line-hash) cursor stored in ingest_state.
func (i *Ingester) RunOnce(ctx context.Context) error {
	i.mu.Lock()
	if i.running {
		i.mu.Unlock()
		return nil
	}
	i.running = true
	i.mu.Unlock()
	defer func() {
		i.mu.Lock()
		i.running = false
		i.mu.Unlock()
	}()

	st, err := i.store.GetIngestState()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	return i.ingestFile(ctx, i.logfile, &st, true)
}

// ImportFile is a one-shot pass over an arbitrary log file (e.g. a rotated
// "Access-old.log"). It does NOT consult or advance the saved cursor — every
// line is offered to the database, and dedup is left to the natural-key
// unique index on sessions and the start_time bound on close events.
func (i *Ingester) ImportFile(ctx context.Context, path string) error {
	i.mu.Lock()
	if i.running {
		i.mu.Unlock()
		return fmt.Errorf("ingest already in progress")
	}
	i.running = true
	i.mu.Unlock()
	defer func() {
		i.mu.Lock()
		i.running = false
		i.mu.Unlock()
	}()

	return i.ingestFile(ctx, path, nil, false)
}

// ingestFile is the shared scanning routine. When advanceCursor is true, the
// state argument's LastTimestamp/LastLineHash bound which lines are skipped,
// and the new cursor is written back at the end. When false, every line is
// processed.
func (i *Ingester) ingestFile(ctx context.Context, path string, st *store.IngestState, advanceCursor bool) error {
	syncStart := time.Now()

	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat logfile: %w", err)
	}
	size := fi.Size()

	var markerTs int64
	var markerHash string
	if advanceCursor && st != nil {
		markerTs = st.LastTimestamp
		markerHash = st.LastLineHash
	}
	// markerFound starts true if we have nothing to look for.
	markerFound := !advanceCursor || markerHash == ""

	i.logger.Info("sync started",
		"logfile", path, "size", size,
		"marker_ts", markerTs, "marker_hash_short", shortHash(markerHash),
		"advance_cursor", advanceCursor)

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open logfile: %w", err)
	}
	defer f.Close()

	batch, err := i.store.BeginIngest()
	if err != nil {
		return fmt.Errorf("begin ingest tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = batch.Rollback()
		}
	}()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	// pending holds events whose timestamp matches markerTs but were
	// encountered before the marker line. If we eventually see the marker
	// they were already processed (drop them); if we hit ts > markerTs
	// without seeing the marker the file rotated and they are new (process).
	type pendingEv struct {
		ev   *parser.Event
		hash string
	}
	var pending []pendingEv

	var lineNum, opens, closes, parseErrs int
	newestTs := markerTs
	newestHash := markerHash

	process := func(ev *parser.Event) {
		switch ev.Type {
		case parser.EventDBOpen:
			ok, err := batch.OpenSession(store.Session{
				StartTime:  ev.Time,
				ClientName: ev.Name,
				ClientID:   ev.ClientID,
				Account:    ev.Account,
				Database:   ev.Database,
				Host:       ev.Host,
				IP:         ev.IP,
				Server:     ev.Server,
			})
			if err != nil {
				i.logger.Warn("open session insert failed", "err", err, "line", lineNum)
			} else if ok {
				opens++
			}
		case parser.EventDBClose:
			closed, err := batch.CloseSession(ev.ClientID, ev.Database, ev.Account, ev.Time)
			if err != nil {
				i.logger.Warn("close session update failed", "err", err, "line", lineNum)
			} else if closed {
				closes++
			}
		}
	}

	for scanner.Scan() {
		if ctx.Err() != nil {
			break
		}
		line := scanner.Bytes()
		lineNum++

		ev, err := parser.ParseLine(string(line))
		if err != nil {
			parseErrs++
			continue
		}
		if ev == nil || (ev.Type != parser.EventDBOpen && ev.Type != parser.EventDBClose) {
			continue
		}

		evTs := ev.Time.Unix()
		sum := sha256.Sum256(line)
		lineHash := hex.EncodeToString(sum[:])

		if advanceCursor && evTs < markerTs {
			continue
		}
		if advanceCursor && evTs == markerTs {
			if !markerFound {
				if lineHash == markerHash {
					markerFound = true
					pending = pending[:0]
					continue
				}
				pending = append(pending, pendingEv{ev: ev, hash: lineHash})
				continue
			}
			// Past the marker, same second: a fresh line.
			process(ev)
			if evTs > newestTs || (evTs == newestTs && lineHash > newestHash) {
				newestTs = evTs
				newestHash = lineHash
			}
			continue
		}
		// evTs > markerTs (or we're not honouring the cursor at all).
		if !markerFound {
			// Marker was never seen — file rotated. Anything we buffered is new.
			markerFound = true
			for _, p := range pending {
				process(p.ev)
				if p.ev.Time.Unix() > newestTs || (p.ev.Time.Unix() == newestTs && p.hash > newestHash) {
					newestTs = p.ev.Time.Unix()
					newestHash = p.hash
				}
			}
			pending = nil
		}
		process(ev)
		if evTs > newestTs || (evTs == newestTs && lineHash > newestHash) {
			newestTs = evTs
			newestHash = lineHash
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan: %w", err)
	}

	// EOF without seeing the marker → rotation; drain pending as new events.
	if !markerFound {
		for _, p := range pending {
			process(p.ev)
			if p.ev.Time.Unix() > newestTs || (p.ev.Time.Unix() == newestTs && p.hash > newestHash) {
				newestTs = p.ev.Time.Unix()
				newestHash = p.hash
			}
		}
	}

	if advanceCursor && st != nil {
		st.LastTimestamp = newestTs
		st.LastLineHash = newestHash
		st.LastRun = time.Now()
		if err := batch.SetIngestState(*st); err != nil {
			return fmt.Errorf("save state: %w", err)
		}
	}
	if err := batch.Commit(); err != nil {
		return fmt.Errorf("commit ingest tx: %w", err)
	}
	committed = true

	i.logger.Info("sync finished",
		"lines", lineNum, "db_opens", opens, "db_closes", closes,
		"parse_errs", parseErrs,
		"marker_ts", newestTs, "marker_hash_short", shortHash(newestHash),
		"duration", time.Since(syncStart).Round(time.Millisecond))
	return nil
}

func shortHash(h string) string {
	if len(h) < 8 {
		return h
	}
	return h[:8]
}
