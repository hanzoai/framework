package framework

// Presence — who is looking at a document right now.
//
// It is deliberately NOT a second realtime mechanism. Presence is a fact about a
// document, so it is announced on the SAME change feed as every other fact about
// that document: joining or leaving appends one row to fw_changes naming the
// watched (doctype, name) with action "present"/"away". A subscriber already
// tailing that DocType learns about it with no new subscription, no new
// transport, and — because the feed filters by the caller's read rights — no
// possibility of learning who is looking at something the caller may not read.
//
// The feed says WHAT changed; the client re-reads the value. Presence follows
// that rule exactly: the change row does not carry the roster, Present does. So
// the change row needs no actor column and a document change and a presence
// change are the same shape.
//
// # Why a store row and not a process map
//
// An in-memory registry only knows the viewers attached to ONE process. Two
// replicas would each show half the room, which is worse than showing none. The
// roster is a TTL row in the shared store — the same reasoning as lock.go — so
// every process reads the whole room. A viewer that crashes is forgotten when
// its lease lapses; there is no cleanup that must succeed.
//
// # Cost
//
// One upsert per viewer per refresh interval (the host refreshes on its
// keep-alive tick, ~25s), and a change row only when the roster actually
// CHANGES — a refresh of an existing presence appends nothing, so a quiet room
// with a hundred viewers is silent.

import (
	"context"
	"fmt"
	"time"

	"github.com/hanzoai/doctype"
)

// Presence actions on the change feed. They name a change to the roster of a
// document, not to the document.
const (
	ChangePresent = "present"
	ChangeAway    = "away"
)

// PresenceTTL is how long an announcement stands without a refresh. It must
// comfortably exceed the host's refresh interval so a live viewer is never
// dropped between ticks.
var PresenceTTL = 90 * time.Second

// Viewer is one live viewer of a document.
type Viewer struct {
	User  string `json:"user"`
	Since int64  `json:"since"`
}

// Announce records that the caller is viewing (dtName, name) for PresenceTTL,
// and reports whether this was a JOIN (the caller was not already present).
//
// It requires the same READ right as fetching the document: you cannot appear in
// a room you may not enter. Re-announcing refreshes the lease and appends
// nothing to the feed, so a heartbeat is silent.
func (e *Engine) Announce(ctx context.Context, c Caller, dtName, name string) (bool, error) {
	if err := e.ready(); err != nil {
		return false, err
	}
	acc, dt, err := e.accessDoc(ctx, c, dtName, doctype.RightRead)
	if err != nil {
		return false, err
	}
	if acc.User == "" {
		return false, fmt.Errorf("%w: presence requires an identified user", ErrForbidden)
	}
	if name = normDocName(dt, name); name == "" {
		return false, doctype.Errorf("document name is required")
	}
	joined, err := e.store.announce(ctx, acc.Org, dt.Name, name, acc.User, PresenceTTL)
	if err != nil {
		return false, err
	}
	if joined {
		if err := e.store.appendPresenceChange(ctx, acc.Org, dt.Name, name, ChangePresent); err != nil {
			return false, err
		}
	}
	return joined, nil
}

// Depart removes the caller from a document's roster and tells the feed. It is
// best-effort by design: a viewer that vanishes without departing is forgotten
// when its lease lapses.
func (e *Engine) Depart(ctx context.Context, c Caller, dtName, name string) error {
	if err := e.ready(); err != nil {
		return err
	}
	acc, dt, err := e.accessDoc(ctx, c, dtName, doctype.RightRead)
	if err != nil {
		return err
	}
	if name = normDocName(dt, name); name == "" {
		return doctype.Errorf("document name is required")
	}
	left, err := e.store.depart(ctx, acc.Org, dt.Name, name, acc.User)
	if err != nil || !left {
		return err
	}
	return e.store.appendPresenceChange(ctx, acc.Org, dt.Name, name, ChangeAway)
}

// Present returns the live roster of (dtName, name) — the caller's org only,
// gated on the same READ right as the document itself.
func (e *Engine) Present(ctx context.Context, c Caller, dtName, name string) ([]Viewer, error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	acc, dt, err := e.accessDoc(ctx, c, dtName, doctype.RightRead)
	if err != nil {
		return nil, err
	}
	if name = normDocName(dt, name); name == "" {
		return nil, doctype.Errorf("document name is required")
	}
	return e.store.present(ctx, acc.Org, dt.Name, name)
}

// normDocName resolves the document a presence call refers to. A Single has
// exactly one document, named for its DocType, so the host need not supply it.
func normDocName(dt DocType, name string) string {
	if dt.IsSingle {
		return dt.Name
	}
	return name
}

// ---- store ----

const presenceDDL = `
CREATE TABLE IF NOT EXISTS fw_presence (
  org        TEXT NOT NULL,
  doctype    TEXT NOT NULL,
  docname    TEXT NOT NULL,
  usr        TEXT NOT NULL,
  since      INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,   -- unix seconds; a lapsed viewer is simply gone
  PRIMARY KEY (org, doctype, docname, usr)
);
CREATE INDEX IF NOT EXISTS ix_fw_presence_expiry ON fw_presence(expires_at);
`

// announce upserts the viewer's lease and reports whether it is new. "New" means
// no LIVE row existed — a lapsed row is a rejoin, and the room should be told.
func (s *Store) announce(ctx context.Context, org, dtName, name, user string, ttl time.Duration) (bool, error) {
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var live int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM fw_presence WHERE org=? AND doctype=? AND docname=? AND usr=? AND expires_at>?`,
		org, dtName, name, user, now).Scan(&live); err != nil {
		return false, fmt.Errorf("presence probe: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO fw_presence (org,doctype,docname,usr,since,expires_at) VALUES (?,?,?,?,?,?)
		 ON CONFLICT(org,doctype,docname,usr) DO UPDATE SET expires_at=excluded.expires_at`,
		org, dtName, name, user, now, now+int64(ttl.Seconds())); err != nil {
		return false, fmt.Errorf("presence upsert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return live == 0, nil
}

func (s *Store) depart(ctx context.Context, org, dtName, name, user string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM fw_presence WHERE org=? AND doctype=? AND docname=? AND usr=?`, org, dtName, name, user)
	if err != nil {
		return false, fmt.Errorf("presence delete: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// present reads the LIVE roster. It filters on expires_at rather than trusting a
// sweep, so a lapsed viewer is never returned even before its row is collected.
func (s *Store) present(ctx context.Context, org, dtName, name string) ([]Viewer, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT usr, since FROM fw_presence
		 WHERE org=? AND doctype=? AND docname=? AND expires_at>? ORDER BY since ASC, usr ASC`,
		org, dtName, name, time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("presence list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]Viewer, 0, 8)
	for rows.Next() {
		var v Viewer
		if err := rows.Scan(&v.User, &v.Since); err != nil {
			return nil, fmt.Errorf("scan viewer: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// appendPresenceChange puts a roster change on the ONE feed, so a subscriber
// already tailing this DocType sees it with no extra subscription. It runs in
// its own transaction (the presence upsert already committed) and wakes the
// same watchers a document write does.
func (s *Store) appendPresenceChange(ctx context.Context, org, dtName, name, action string) error {
	if err := s.appendChange(ctx, s.db, org, dtName, name, action, 0); err != nil {
		return err
	}
	s.committed(ctx)
	return nil
}

// sweepPresence collects lapsed rows. Housekeeping only: present() already
// filters by expiry, so a missed sweep is a storage cost, never a wrong answer.
// It rides the change log's trim interval — one timer for both.
func (s *Store) sweepPresence(ctx context.Context) {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM fw_presence WHERE expires_at < ?`, time.Now().Unix())
}
