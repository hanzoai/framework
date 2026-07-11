package framework

import (
	"context"
	"testing"
	"time"
)

// red_lease_final_test.go — RED final-pass attacks on the store lease primitive that
// Publish's TOCTOU interlock is built on. It re-derives, from the outside, the two
// properties the double-post safety hinges on:
//
//   1. Key namespacing survives an embedded NUL (the delimiter Publish uses). If the
//      SQL driver truncated a TEXT bind at the first \x00, EVERY publish lease key
//      would collapse to the "content.publish" prefix — one global lease serializing
//      (and starving) all items across all doctypes. Proven false here on whatever
//      driver the build selects (modernc under CGO=0, mattn under -race/CGO=1).
//
//   2. An EXPIRED-but-unreleased holder IS preempted. This is the mechanism behind the
//      Vector-1 finding: the lease is only a crash net, NOT a fence. A holder still
//      mid-critical-section when its TTL lapses is stolen by a contender — so if the
//      content fan-out ever runs longer than publishLeaseTTL, the stealer re-reads an
//      as-yet-unrecorded (empty) skip-set and re-posts. The fix is holder renewal;
//      this test pins the exact primitive behavior that makes renewal necessary.

// TestRedLease_EmbeddedNULKeysDoNotCollide proves the driver preserves embedded NUL in
// a lockkey, so keys that differ only after a \x00 are independent leases — no truncation
// collapse, no cross-item starvation.
func TestRedLease_EmbeddedNULKeysDoNotCollide(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const org = "acme"

	// Three keys sharing the "content.publish" prefix, differing only AFTER embedded NULs.
	// A C-string-truncating driver would treat all three as "content.publish" and let only
	// the first win. They MUST each acquire independently.
	keys := []string{
		"content.publish\x00SocialPost\x00A",
		"content.publish\x00SocialPost\x00B",
		"content.publish\x00SocialPost\x00A\x00suffix",
	}
	for i, k := range keys {
		ok, err := s.acquireLock(ctx, org, k, "h", time.Minute)
		if err != nil {
			t.Fatalf("acquire %d (%q): %v", i, k, err)
		}
		if !ok {
			t.Fatalf("KEY COLLAPSE: lockkey %q did not acquire independently — the driver "+
				"likely truncated at the embedded NUL, collapsing distinct publish leases", k)
		}
	}

	// Sanity in the other direction: the SAME NUL-bearing key still mutually excludes, so
	// the independence above is real namespacing, not an "everything wins" bug.
	if ok, err := s.acquireLock(ctx, org, keys[0], "h2", time.Minute); err != nil || ok {
		t.Fatalf("a live NUL-bearing lease must still exclude a second holder: ok=%v err=%v", ok, err)
	}
}

// TestRedLease_ExpiredLiveHolderIsPreempted is the Vector-1 mechanism proof: a holder that
// has NOT released but whose TTL lapsed is stolen. Combined with publish.go recording
// external_ids only AFTER the whole fan-out, this is exactly how a fan-out longer than
// publishLeaseTTL re-opens the double-post window. The lease is a crash net, not a fence.
func TestRedLease_ExpiredLiveHolderIsPreempted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const org, key = "acme", "content.publish\x00SocialPost\x00SLOW"

	// Holder A takes a SHORT lease and never releases — it models a publisher still mid
	// fan-out when its lease lapses.
	if ok, err := s.acquireLock(ctx, org, key, "holderA", 40*time.Millisecond); err != nil || !ok {
		t.Fatalf("holderA acquire: ok=%v err=%v", ok, err)
	}
	// While A's lease is live, a contender must LOSE (no premature steal).
	if ok, _ := s.acquireLock(ctx, org, key, "holderB", time.Minute); ok {
		t.Fatal("a LIVE lease must never be stolen")
	}

	time.Sleep(70 * time.Millisecond) // A is STILL 'working' (never released) but TTL lapsed.

	// Contender B now steals the expired lease even though A is still mid-flight. THIS is
	// the double-post door: B will re-read the not-yet-recorded skip-set and re-fan-out.
	if ok, err := s.acquireLock(ctx, org, key, "holderB", time.Minute); err != nil || !ok {
		t.Fatalf("PREEMPTION EXPECTED: an expired-but-unreleased holder must be preemptible "+
			"(that is the finding) — ok=%v err=%v", ok, err)
	}
	// A's late release must be a holder-scoped no-op: it cannot free B's fresh lease (so at
	// least the crash-safety half holds — a stale holder never double-frees).
	if err := s.releaseLock(ctx, org, key, "holderA"); err != nil {
		t.Fatalf("stale release: %v", err)
	}
	if ok, _ := s.acquireLock(ctx, org, key, "holderC", time.Minute); ok {
		t.Fatal("holderB's fresh lease must survive holderA's stale release")
	}
}
