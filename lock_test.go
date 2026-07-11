package framework

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lock_test.go proves the store lease primitive at the level Publish depends on:
// exactly one holder at a time, a LIVE lease is never stolen, an EXPIRED lease is
// reclaimed, release is holder-scoped, and different (org,key) never contend.

func TestLock_MutualExclusion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const org, key = "acme", "content.publish\x00SocialPost\x00POST-1"

	ok, err := s.acquireLock(ctx, org, key, "holderA", time.Minute)
	if err != nil || !ok {
		t.Fatalf("first acquire must win: ok=%v err=%v", ok, err)
	}
	// A second, different holder must LOSE while A's lease is live.
	if ok, err := s.acquireLock(ctx, org, key, "holderB", time.Minute); err != nil || ok {
		t.Fatalf("second acquire on a live lease must lose: ok=%v err=%v", ok, err)
	}
	// A holder for a DIFFERENT key is independent.
	if ok, err := s.acquireLock(ctx, org, key+"-other", "holderB", time.Minute); err != nil || !ok {
		t.Fatalf("different key must acquire independently: ok=%v err=%v", ok, err)
	}
	// A holder in a DIFFERENT org is independent (tenant isolation).
	if ok, err := s.acquireLock(ctx, "other-org", key, "holderB", time.Minute); err != nil || !ok {
		t.Fatalf("different org must acquire independently: ok=%v err=%v", ok, err)
	}
}

func TestLock_HolderScopedRelease(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const org, key = "acme", "k"

	if ok, _ := s.acquireLock(ctx, org, key, "holderA", time.Minute); !ok {
		t.Fatal("acquire A")
	}
	// A NON-holder release must not free A's lease.
	if err := s.releaseLock(ctx, org, key, "holderB"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ok, _ := s.acquireLock(ctx, org, key, "holderC", time.Minute); ok {
		t.Fatal("a non-holder release must NOT free a live lease")
	}
	// The real holder releases; now the key is free.
	if err := s.releaseLock(ctx, org, key, "holderA"); err != nil {
		t.Fatalf("release A: %v", err)
	}
	if ok, _ := s.acquireLock(ctx, org, key, "holderC", time.Minute); !ok {
		t.Fatal("after the holder released, the key must be acquirable")
	}
}

func TestLock_ExpiredLeaseReclaimed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const org, key = "acme", "k"

	// Acquire with an already-lapsed TTL: the row exists but is expired.
	if ok, _ := s.acquireLock(ctx, org, key, "crashed", -time.Second); !ok {
		t.Fatal("acquire with negative ttl still writes the (expired) row")
	}
	// A fresh acquirer reclaims the expired lease (a crashed holder never wedges).
	if ok, err := s.acquireLock(ctx, org, key, "recoverer", time.Minute); err != nil || !ok {
		t.Fatalf("expired lease must be reclaimable: ok=%v err=%v", ok, err)
	}
	// The stale (crashed) holder's release must NOT free the recoverer's live lease.
	if err := s.releaseLock(ctx, org, key, "crashed"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ok, _ := s.acquireLock(ctx, org, key, "other", time.Minute); ok {
		t.Fatal("the recoverer's live lease must survive the stale holder's release")
	}
}

// TestLock_ConcurrentAcquireExactlyOne fires N goroutines at one key; exactly one wins.
func TestLock_ConcurrentAcquireExactlyOne(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const org, key = "acme", "k"

	const n = 32
	var wins int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ok, err := s.acquireLock(ctx, org, key, holderID(i), time.Minute)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			if ok {
				atomic.AddInt64(&wins, 1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if wins != 1 {
		t.Fatalf("exactly one goroutine must win the lease, got %d", wins)
	}
}

func holderID(i int) string { return "h" + string(rune('A'+i%26)) + string(rune('0'+i/26)) }
