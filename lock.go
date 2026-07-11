package framework

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// lock.go is the framework store's ONE coordination primitive: an exclusive,
// TTL-bounded, cross-process lease keyed by (org, key), held as a row in fw_locks.
//
// # Why a store lease and not a mutex
//
// A first-party subsystem sometimes wraps a NON-idempotent side effect that must run
// at most once per item even when several drivers fire it at once — the content
// lane's channel fan-out is the motivating case (console + /v1/automations + MCP +
// a bot all call the same Publish). An in-memory mutex only serializes ONE process;
// two pods publishing the SAME item would each hold their own mutex and both post.
// The lease lives in the shared store, so contenders across the fleet contend on the
// SAME row — the interlock is correct wherever the store is (single-writer SQLite
// today, a shared SQL store tomorrow).
//
// # Fail-safe
//
// A lease AUTO-EXPIRES (expires_at). A holder that crashes mid-critical-section never
// wedges the item: the next acquirer reclaims the lapsed row. Release is
// holder-scoped, so a stale holder whose lease was already reclaimed can never delete
// the new holder's row. The caller picks a `ttl` that comfortably exceeds the guarded
// section (so a slow-but-live holder is not pre-empted) and a `wait` budget after
// which contention is answered honestly ("in progress") rather than as a failure.

// leasePollInterval is the backoff between acquire attempts while a contender waits
// for a live holder to release. Short enough that a fast holder's release is picked
// up promptly; long enough that polling is not a busy-loop.
const leasePollInterval = 20 * time.Millisecond

// Lease is an acquired exclusive claim on (org, key). Release it when the guarded
// critical section completes; the TTL is only the crash safety net.
type Lease struct {
	store  *Store
	org    string
	key    string
	holder string
}

// AcquireLease takes an exclusive lease on (org, key) for up to `ttl`, polling with
// bounded backoff until it wins or `wait` elapses. It returns:
//
//   - (lease, true, nil)  — acquired; the caller owns the critical section and MUST
//     Release when done (a defer is idiomatic);
//   - (nil, false, nil)   — a LIVE lease stayed held by someone else for the whole
//     `wait` window; the caller answers an honest "in progress"/"retry", never a 5xx;
//   - (nil, false, err)   — a genuine store failure (or ctx cancellation).
//
// `ttl` MUST exceed the guarded section so a live holder is never pre-empted mid-flight.
func AcquireLease(ctx context.Context, org, key string, ttl, wait time.Duration) (*Lease, bool, error) {
	s := mounted
	if s == nil || s.State.store == nil {
		return nil, false, fmt.Errorf("framework: not mounted")
	}
	if org == "" || key == "" {
		return nil, false, fmt.Errorf("framework.AcquireLease: empty org/key")
	}
	holder, err := newHolderToken()
	if err != nil {
		return nil, false, err
	}
	deadline := time.Now().Add(wait)
	for {
		ok, err := s.State.store.acquireLock(ctx, org, key, holder, ttl)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return &Lease{store: s.State.store, org: org, key: key, holder: holder}, true, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, false, nil // held by a live owner for the whole window
		}
		sleep := leasePollInterval
		if sleep > remaining {
			sleep = remaining
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(sleep):
		}
	}
}

// Release frees the lease IFF this holder still owns it. Best-effort and idempotent:
// a lease already reclaimed by TTL is a no-op (holder-scoped delete), and a store
// error is returned for the caller to log — never to fail the already-completed work.
// Release with a detached context so a cancelled request still frees the row promptly
// (rather than leaving it to TTL).
func (l *Lease) Release(ctx context.Context) error {
	if l == nil || l.store == nil {
		return nil
	}
	return l.store.releaseLock(context.WithoutCancel(ctx), l.org, l.key, l.holder)
}

// newHolderToken mints a random, unguessable holder id so releases are provably
// scoped to the acquisition that took the lease (never a colliding constant).
func newHolderToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("lease holder token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ---- store methods (the atomic acquire/release, portable across SQL backends) ----

// acquireLock atomically claims (org, lockkey) for `holder` until now+ttl. It wins
// when the row is ABSENT or its lease has EXPIRED; it loses (false) when a LIVE lease
// is held by someone else. Two statements — no reliance on UPSERT-with-WHERE row-count
// quirks — so the semantics are identical on single-writer SQLite and a concurrent SQL
// store:
//
//	(1) UPDATE ... WHERE expires_at<=now  — steal an EXPIRED (or refresh our OWN) lease.
//	    Matches nothing when a LIVE lease holds the row, so a live holder is never stolen.
//	(2) else INSERT — a fresh key wins; a PRIMARY-KEY conflict means a LIVE lease already
//	    holds it → not acquired. Under a race two inserters collide on the PK and exactly
//	    one wins.
func (s *Store) acquireLock(ctx context.Context, org, lockkey, holder string, ttl time.Duration) (bool, error) {
	now := time.Now().UnixNano()
	expires := now + int64(ttl)
	res, err := s.db.ExecContext(ctx,
		`UPDATE fw_locks SET holder=?, expires_at=? WHERE org=? AND lockkey=? AND expires_at<=?`,
		holder, expires, org, lockkey, now)
	if err != nil {
		return false, fmt.Errorf("steal lock: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO fw_locks (org, lockkey, holder, expires_at) VALUES (?,?,?,?)`,
		org, lockkey, holder, expires)
	if err == nil {
		return true, nil
	}
	if isUnique(err) {
		return false, nil // a live lease already holds the key
	}
	return false, fmt.Errorf("acquire lock: %w", err)
}

// releaseLock frees (org, lockkey) IFF `holder` still owns it. Holder-scoping means a
// holder whose lease was already reclaimed by TTL cannot delete the new owner's row.
func (s *Store) releaseLock(ctx context.Context, org, lockkey, holder string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM fw_locks WHERE org=? AND lockkey=? AND holder=?`, org, lockkey, holder); err != nil {
		return fmt.Errorf("release lock: %w", err)
	}
	return nil
}
