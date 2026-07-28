package framework

// The change feed — ONE mechanism by which any interested party learns that a
// document changed, for EVERY DocType, with no per-DocType registration.
//
// # Why it lives in the engine
//
// The engine is the only thing that knows a write COMMITTED. A change recorded
// anywhere else would be a guess: a lifecycle Hook is keyed by DocType name, so
// it cannot cover a DocType defined at runtime through the CollectionBuilder,
// and the delete/submit/cancel paths have no after-phase at all. So the append
// happens where the fact is produced — inside the same SQLite transaction as
// the state change it describes. A change row exists if and only if the write
// it names landed.
//
// # The value
//
// A Change is a FACT: "at seq N, in org O, document D of DocType T changed by
// action A, leaving it at docstatus S at time At". It carries no payload. A
// subscriber that cares reads the document through the ordinary permission-
// checked GetDocument; the feed never becomes a second, weaker read path.
//
// # Ordering and resume
//
// seq is a single AUTOINCREMENT column over the whole table, so it is a total
// order across every org and every DocType, and it never repeats even after
// trimming (sqlite_sequence keeps the high-water mark). That makes it a valid
// cursor: a client that has seen seq N asks for > N and misses nothing. The
// order is the COMMIT order of this store, which is the only order that exists
// (MaxOpenConns(1) serialises writers).
//
// # Retention
//
// The log is trimmed by age (Retention, default 24h). A client whose cursor has
// fallen behind what is still retained is told so — ChangeFeed.Reset — rather
// than being silently handed a gap; it refetches and resumes. Retention is
// bounded work: one DELETE per trimInterval, not one per append.
//
// # Tenancy
//
// Every query is scoped by the caller's VALIDATED org, and then by the DocTypes
// that caller may READ — the same doctype.Grants calculus the document routes
// use. Nothing else can enter the feed. A caller with no validated org is
// refused before any store access, exactly as every other operation is.
//
// # Fan-out
//
// Delivery is NOT in this file and not in this process. The durable log IS the
// fan-out: any process holding this database serves any subscriber by reading
// rows > cursor. WatchChanges only removes latency for subscribers that happen
// to share the writer's process; a subscriber elsewhere still sees every change
// on its next read. Correctness never depends on a message reaching anybody.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hanzoai/doctype"
)

// The actions a Change can report. They name what HAPPENED to the document, not
// which engine method was called, so a subscriber switches on a closed set.
const (
	ChangeCreated   = "created"
	ChangeUpdated   = "updated"
	ChangeSubmitted = "submitted"
	ChangeCancelled = "cancelled"
	ChangeDeleted   = "deleted"
)

// Retention is how long a change row is kept. A subscriber may be disconnected
// for up to this long and still resume exactly; longer, and it is told to reset.
// A package var so a test can shrink it — never a per-call knob, because two
// callers disagreeing about retention would give one of them a silent gap.
var Retention = 24 * time.Hour

// trimInterval bounds retention work: the trim runs at most this often per
// store, not once per append.
var trimInterval = time.Minute

// changeLimit is the default and maximum page size of a Changes read.
const (
	changeLimit    = 200
	maxChangeLimit = 1000
)

// Change is one committed state change to one document.
type Change struct {
	Seq       int64  `json:"seq"`
	DocType   string `json:"doctype"`
	Module    string `json:"module,omitempty"`
	Name      string `json:"name"`
	Action    string `json:"action"`
	DocStatus int    `json:"docstatus"`
	At        int64  `json:"at"`
}

// ChangesQuery selects a page of the caller's feed.
//
// DocTypes and Modules NARROW what the caller already may read; they never
// widen it. Empty means every DocType the caller may read.
type ChangesQuery struct {
	// Since is an exclusive cursor: rows with seq > Since.
	Since int64
	// DocTypes names DocTypes of interest.
	DocTypes []string
	// Modules names modules of interest; their DocTypes are resolved per read,
	// so a DocType added to a module reaches a subscriber already connected.
	Modules []string
	// Limit bounds the page (default changeLimit, capped at maxChangeLimit).
	Limit int
}

// ChangeFeed is one page of the feed.
type ChangeFeed struct {
	// Changes is the visible page, ascending by Seq. It may be empty while
	// Cursor still advances — invisible or filtered rows are skipped, not resent.
	Changes []Change `json:"changes"`
	// Cursor is what to pass as the next Since. It advances past rows the caller
	// could not see, so a caller subscribed to a quiet DocType in a busy org does
	// not rescan the same range forever.
	Cursor int64 `json:"cursor"`
	// Reset reports that Since fell behind retention: rows between it and the
	// oldest retained row are gone. The caller must refetch current state, then
	// resume from Cursor. It is never silently omitted.
	Reset bool `json:"reset,omitempty"`
}

// ChangeCursor returns the feed's current high-water mark for a validated
// caller: the cursor meaning "everything from now on, nothing before". A client
// that renders current state from a list call starts here, so it sees each
// change exactly once.
func (e *Engine) ChangeCursor(ctx context.Context, c Caller) (int64, error) {
	if err := e.ready(); err != nil {
		return 0, err
	}
	if _, err := e.resolve(ctx, c); err != nil {
		return 0, err
	}
	_, high, err := e.store.changeWatermarks(ctx)
	return high, err
}

// Changes returns the next page of the caller's feed.
//
// It is the ONE read of the change log. The SSE stream is a loop over this
// function and the polling route is a single call to it, so a client that
// cannot hold a connection open sees exactly what a streaming one sees.
func (e *Engine) Changes(ctx context.Context, c Caller, q ChangesQuery) (ChangeFeed, error) {
	if err := e.ready(); err != nil {
		return ChangeFeed{}, err
	}
	acc, err := e.resolve(ctx, c)
	if err != nil {
		return ChangeFeed{}, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = changeLimit
	}
	if limit > maxChangeLimit {
		limit = maxChangeLimit
	}

	// The visible set is resolved on EVERY read, not once per connection: a
	// DocType defined a moment ago, or a role granted a moment ago, takes effect
	// on the next page rather than on the next reconnect.
	visible, err := e.visibleDocTypes(ctx, acc, q)
	if err != nil {
		return ChangeFeed{}, err
	}

	retainedFrom, high, err := e.store.changeWatermarks(ctx)
	if err != nil {
		return ChangeFeed{}, err
	}
	feed := ChangeFeed{Changes: []Change{}, Cursor: q.Since}
	// A cursor older than the oldest retained row means rows between them were
	// trimmed. Say so instead of handing back a gap.
	if q.Since > 0 && retainedFrom > q.Since+1 {
		feed.Reset = true
	}
	if len(visible) == 0 {
		feed.Cursor = max64(q.Since, high)
		return feed, nil
	}

	names := make([]string, 0, len(visible))
	for n := range visible {
		names = append(names, n)
	}
	rows, err := e.store.changes(ctx, acc.Org, q.Since, names, limit)
	if err != nil {
		return ChangeFeed{}, err
	}
	for i := range rows {
		rows[i].Module = visible[rows[i].DocType]
	}
	feed.Changes = rows
	if len(rows) > 0 {
		feed.Cursor = rows[len(rows)-1].Seq
	}
	// A short page means the scan reached the end of the log, so every row up to
	// the high-water mark has been considered — invisible ones included.
	if len(rows) < limit {
		feed.Cursor = max64(feed.Cursor, high)
	}
	return feed, nil
}

// visibleDocTypes is the feed's authorization: the DocTypes this caller may
// READ, narrowed by the query's DocTypes/Modules. It returns name → module.
//
// It reuses Access.Can, the same calculus GetDocument enforces, so the feed can
// never disclose the existence of a document the caller could not fetch.
func (e *Engine) visibleDocTypes(ctx context.Context, acc Access, q ChangesQuery) (map[string]string, error) {
	dts, err := e.store.ListDocTypes(ctx, acc.Org)
	if err != nil {
		return nil, err
	}
	wantDT := lowerSet(q.DocTypes)
	wantMod := lowerSet(q.Modules)
	out := make(map[string]string, len(dts))
	for i := range dts {
		dt := &dts[i]
		if !acc.Can(dt, doctype.RightRead) {
			continue
		}
		if len(wantDT) > 0 || len(wantMod) > 0 {
			if !wantDT[strings.ToLower(dt.Name)] && !wantMod[strings.ToLower(dt.Module)] {
				continue
			}
		}
		out[dt.Name] = dt.Module
	}
	return out, nil
}

// WatchChanges returns a channel that receives a wake whenever this process
// commits a change, and a function that stops watching. It is a LATENCY
// optimisation, never a delivery guarantee: the channel carries no data and
// coalesces (a single-slot buffer with a non-blocking send), so a slow watcher
// can neither block a writer nor queue unboundedly. What a watcher does on wake
// is read Changes — which is also what it must do on a timer, because a write
// in ANOTHER process rings no channel here.
func (e *Engine) WatchChanges() (<-chan struct{}, func()) {
	if e == nil || e.store == nil {
		ch := make(chan struct{})
		close(ch)
		return ch, func() {}
	}
	return e.store.watch()
}

// ---- store ----

// changesDDL is the log's schema. seq is AUTOINCREMENT so the cursor space is
// never reused after a trim: sqlite_sequence keeps the high-water mark, which is
// what lets a resume distinguish "nothing happened" from "your window is gone".
const changesDDL = `
CREATE TABLE IF NOT EXISTS fw_changes (
  seq       INTEGER PRIMARY KEY AUTOINCREMENT,
  org       TEXT NOT NULL,
  doctype   TEXT NOT NULL,
  name      TEXT NOT NULL,
  action    TEXT NOT NULL,
  docstatus INTEGER NOT NULL DEFAULT 0,
  at        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_fw_changes_org_seq ON fw_changes(org, seq);
CREATE INDEX IF NOT EXISTS ix_fw_changes_at ON fw_changes(at);
`

// appendChange records one committed state change. It takes an execer so the
// caller passes the SAME transaction as the write, making the row and the state
// change atomic together.
func (s *Store) appendChange(ctx context.Context, x execer, org, dtName, name, action string, docstatus int) error {
	if _, err := x.ExecContext(ctx,
		`INSERT INTO fw_changes (org,doctype,name,action,docstatus,at) VALUES (?,?,?,?,?,?)`,
		org, dtName, name, action, docstatus, time.Now().Unix()); err != nil {
		return fmt.Errorf("append change: %w", err)
	}
	return nil
}

// committed is what a write calls AFTER its transaction commits: trim the log if
// due, then wake this process's watchers. Never call it before the commit — a
// watcher woken early would query and not find the row.
func (s *Store) committed(ctx context.Context) {
	s.trim(ctx)
	s.signal()
}

// trim enforces Retention at most once per trimInterval.
func (s *Store) trim(ctx context.Context) {
	s.watchMu.Lock()
	if time.Since(s.lastTrim) < trimInterval {
		s.watchMu.Unlock()
		return
	}
	s.lastTrim = time.Now()
	s.watchMu.Unlock()
	cutoff := time.Now().Add(-Retention).Unix()
	// Retention is housekeeping: a failure must never fail the write that already
	// landed, so both sweeps ignore their errors and the next tick tries again.
	_, _ = s.db.ExecContext(ctx, `DELETE FROM fw_changes WHERE at < ?`, cutoff)
	s.sweepPresence(ctx)
}

// changes reads one page of the org's log restricted to dtNames.
func (s *Store) changes(ctx context.Context, org string, since int64, dtNames []string, limit int) ([]Change, error) {
	if len(dtNames) == 0 {
		return []Change{}, nil
	}
	args := make([]any, 0, len(dtNames)+3)
	args = append(args, org, since)
	holes := make([]string, len(dtNames))
	for i, n := range dtNames {
		holes[i] = "?"
		args = append(args, n)
	}
	args = append(args, limit)
	q := `SELECT seq,doctype,name,action,docstatus,at FROM fw_changes
	      WHERE org=? AND seq>? AND doctype IN (` + strings.Join(holes, ",") + `)
	      ORDER BY seq ASC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list changes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]Change, 0, limit)
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.Seq, &c.DocType, &c.Name, &c.Action, &c.DocStatus, &c.At); err != nil {
			return nil, fmt.Errorf("scan change: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// changeWatermarks returns (retainedFrom, high):
//
//	retainedFrom  the oldest seq still in the log; when the log is empty it is
//	              high+1, i.e. "nothing at all is retained".
//	high          the largest seq ever assigned (sqlite_sequence), which never
//	              goes backwards when rows are trimmed.
//
// Both are GLOBAL, not per-org, because seq is global: comparing a client's
// cursor against a per-org minimum would call a quiet org's valid cursor stale.
func (s *Store) changeWatermarks(ctx context.Context) (int64, int64, error) {
	var high sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name='fw_changes'`).Scan(&high)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, fmt.Errorf("change high-water: %w", err)
	}
	var oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(seq) FROM fw_changes`).Scan(&oldest); err != nil {
		return 0, 0, fmt.Errorf("change oldest: %w", err)
	}
	if oldest.Valid {
		return oldest.Int64, high.Int64, nil
	}
	return high.Int64 + 1, high.Int64, nil
}

// watch registers a wake channel and returns it with its cancel.
func (s *Store) watch() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.watchMu.Lock()
	if s.watchers == nil {
		s.watchers = map[int64]chan struct{}{}
	}
	s.nextWatcher++
	id := s.nextWatcher
	s.watchers[id] = ch
	s.watchMu.Unlock()
	return ch, func() {
		s.watchMu.Lock()
		delete(s.watchers, id)
		s.watchMu.Unlock()
	}
}

// signal wakes every watcher without blocking on any of them.
func (s *Store) signal() {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	for _, ch := range s.watchers {
		select {
		case ch <- struct{}{}:
		default: // already dirty; one wake is enough
		}
	}
}

// watchState is the Store's change-feed bookkeeping, embedded so store.go's
// struct literal keeps working and the zero value is usable.
type watchState struct {
	watchMu     sync.Mutex
	watchers    map[int64]chan struct{}
	nextWatcher int64
	lastTrim    time.Time
}

// docStatusAction names the docstatus transition a submit/cancel performed. The
// engine has exactly two, so this is total.
func docStatusAction(to int) string {
	if to == 1 {
		return ChangeSubmitted
	}
	return ChangeCancelled
}

func lowerSet(in []string) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out[s] = true
		}
	}
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
