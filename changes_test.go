package framework

// changes_test.go proves the four properties the change feed is for:
//
//	COMPLETE   every committed write of every DocType produces exactly one row;
//	ATOMIC     a write that did not land produces none;
//	SCOPED     a caller sees only its own org, and only DocTypes it may read;
//	RESUMABLE  a cursor replays without gaps or duplicates, and says so when the
//	           retention window has passed it by.
//
// TestChanges_TenantIsolation is the load-bearing one: it is constructed so that
// deleting the org predicate from the feed query makes it FAIL, not merely
// weaken. Both orgs use the SAME DocType name, so the doctype filter cannot
// stand in for tenancy.

import (
	"context"
	"testing"
	"time"
)

func ticketDT() DocType {
	return DocType{
		Name: "Ticket", IsSubmittable: true, TitleField: "subject",
		Fields: []DocField{
			{Fieldname: "subject", Fieldtype: FieldData, Reqd: true},
			{Fieldname: "status", Fieldtype: FieldData},
		},
		Perms: []DocPerm{
			{Role: RoleSystemManager, Read: true, Write: true, Create: true, Delete: true, Submit: true, Cancel: true},
			{Role: "Agent", Read: true, Write: true, Create: true},
		},
	}
}

// feed reads the caller's whole feed from `since` in one page.
func feed(t *testing.T, e *Engine, c Caller, since int64) ChangeFeed {
	t.Helper()
	f, err := e.Changes(context.Background(), c, ChangesQuery{Since: since})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	return f
}

func actions(f ChangeFeed) []string {
	out := make([]string, 0, len(f.Changes))
	for _, c := range f.Changes {
		out = append(out, c.Action)
	}
	return out
}

// TestChanges_EveryWritePathAppends: create, update, submit, cancel and delete
// each produce exactly one change, in commit order, naming what happened.
func TestChanges_EveryWritePathAppends(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	o := owner("acme")
	seed(t, e, "acme", ticketDT())

	doc, err := e.CreateDocument(ctx, o, "Ticket", map[string]any{"subject": "printer on fire"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := e.UpdateDocument(ctx, o, "Ticket", doc.Name, map[string]any{"subject": "printer on fire", "status": "open"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := e.Submit(ctx, o, "Ticket", doc.Name); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := e.Cancel(ctx, o, "Ticket", doc.Name); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := e.DeleteDocument(ctx, o, "Ticket", doc.Name); err != nil {
		t.Fatalf("delete: %v", err)
	}

	f := feed(t, e, o, 0)
	want := []string{ChangeCreated, ChangeUpdated, ChangeSubmitted, ChangeCancelled, ChangeDeleted}
	got := actions(f)
	if len(got) != len(want) {
		t.Fatalf("got %d changes %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("change %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
	for i, c := range f.Changes {
		if c.DocType != "Ticket" || c.Name != doc.Name {
			t.Fatalf("change %d = %+v, want doctype Ticket name %q", i, c, doc.Name)
		}
		if i > 0 && c.Seq <= f.Changes[i-1].Seq {
			t.Fatalf("seq not strictly increasing at %d: %d after %d", i, c.Seq, f.Changes[i-1].Seq)
		}
	}
	// docstatus rides along so a queue can re-sort without re-reading.
	if f.Changes[2].DocStatus != 1 || f.Changes[3].DocStatus != 2 {
		t.Fatalf("submit/cancel docstatus = %d/%d, want 1/2", f.Changes[2].DocStatus, f.Changes[3].DocStatus)
	}
}

// TestChanges_SingleAndModule: a Single DocType's upsert is reported, and the
// module a DocType belongs to travels with the change so a module subscriber can
// route it without a second lookup.
func TestChanges_SingleAndModule(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	o := owner("acme")
	seed(t, e, "acme", DocType{
		Name: "Support Settings", Module: "help", IsSingle: true,
		Fields: []DocField{{Fieldname: "sla_hours", Fieldtype: FieldInt}},
		Perms:  []DocPerm{{Role: RoleSystemManager, Read: true, Write: true, Create: true}},
	})
	if _, err := e.CreateDocument(ctx, o, "Support Settings", map[string]any{"sla_hours": 4.0}); err != nil {
		t.Fatalf("upsert single: %v", err)
	}
	f := feed(t, e, o, 0)
	if len(f.Changes) != 1 || f.Changes[0].Action != ChangeUpdated {
		t.Fatalf("single feed = %+v, want one %q", f.Changes, ChangeUpdated)
	}
	if f.Changes[0].Module != "help" {
		t.Fatalf("module = %q, want help", f.Changes[0].Module)
	}
}

// TestChanges_TenantIsolation is the isolation gate.
//
// Two orgs define a DocType with the SAME name and both write. Each org's feed
// must contain ONLY its own rows. Because the names collide, the doctype filter
// cannot mask a missing org predicate: remove `org=?` from Store.changes and
// this test fails immediately with the other tenant's documents in hand.
func TestChanges_TenantIsolation(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	acme, globex := owner("acme"), owner("globex")
	seed(t, e, "acme", ticketDT())
	seed(t, e, "globex", ticketDT())

	// Interleave the writes so the two orgs' rows are adjacent in the log: a feed
	// that scanned by seq alone would pick up its neighbour.
	for i := 0; i < 3; i++ {
		if _, err := e.CreateDocument(ctx, acme, "Ticket", map[string]any{"subject": "acme-secret"}); err != nil {
			t.Fatalf("acme create: %v", err)
		}
		if _, err := e.CreateDocument(ctx, globex, "Ticket", map[string]any{"subject": "globex-secret"}); err != nil {
			t.Fatalf("globex create: %v", err)
		}
	}

	for _, tc := range []struct {
		who    Caller
		docs   int
		absent string
	}{
		{acme, 3, "globex"},
		{globex, 3, "acme"},
	} {
		f := feed(t, e, tc.who, 0)
		if len(f.Changes) != tc.docs {
			t.Fatalf("%s sees %d changes, want %d: %+v", tc.who.Org, len(f.Changes), tc.docs, f.Changes)
		}
		for _, c := range f.Changes {
			// Document names are per-org autonames; the sibling org's documents are
			// identifiable only by reading them, so assert on what leaked structurally:
			// every visible row must belong to a document THIS org can fetch.
			if _, err := e.GetDocument(ctx, tc.who, c.DocType, c.Name); err != nil {
				t.Fatalf("%s: feed disclosed %s/%s which it cannot read: %v", tc.who.Org, c.DocType, c.Name, err)
			}
		}
		// And the total must not equal the whole log — that would mean no scoping.
		if len(f.Changes) == 6 {
			t.Fatalf("%s sees the entire log (6 rows): org scoping is not applied", tc.who.Org)
		}
	}
}

// TestChanges_NoTenantRefused: a caller with no validated org never reaches the
// store — the same refusal every other engine operation makes.
func TestChanges_NoTenantRefused(t *testing.T) {
	e := testEngine(t)
	seed(t, e, "acme", ticketDT())
	if _, err := e.CreateDocument(context.Background(), owner("acme"), "Ticket", map[string]any{"subject": "x"}); err != nil {
		t.Fatalf("seed doc: %v", err)
	}
	if _, err := e.Changes(context.Background(), Caller{}, ChangesQuery{}); err == nil {
		t.Fatal("Changes with no validated org must be refused")
	} else if Classify(err) != CodeForbidden {
		t.Fatalf("Classify = %v, want %v", Classify(err), CodeForbidden)
	}
	if _, err := e.ChangeCursor(context.Background(), Caller{}); err == nil {
		t.Fatal("ChangeCursor with no validated org must be refused")
	}
}

// TestChanges_ReadPermissionFilters: a member holding a role with no read on a
// DocType is not told that its documents exist — while the cursor still advances
// past them, so the member does not rescan the same range forever.
func TestChanges_ReadPermissionFilters(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	o := owner("acme")
	seed(t, e, "acme", ticketDT())
	seed(t, e, "acme", DocType{
		Name: "Payroll Run",
		Fields: []DocField{
			{Fieldname: "amount", Fieldtype: FieldCurrency},
		},
		Perms: []DocPerm{{Role: RoleSystemManager, Read: true, Write: true, Create: true}},
	})
	agent := member("acme")
	if _, err := e.AssignRole(ctx, o, agent.User, "Agent"); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	if _, err := e.CreateDocument(ctx, o, "Payroll Run", map[string]any{"amount": 1000.0}); err != nil {
		t.Fatalf("payroll: %v", err)
	}
	if _, err := e.CreateDocument(ctx, o, "Ticket", map[string]any{"subject": "visible"}); err != nil {
		t.Fatalf("ticket: %v", err)
	}
	if _, err := e.CreateDocument(ctx, o, "Payroll Run", map[string]any{"amount": 2000.0}); err != nil {
		t.Fatalf("payroll 2: %v", err)
	}

	f := feed(t, e, agent, 0)
	if len(f.Changes) != 1 || f.Changes[0].DocType != "Ticket" {
		t.Fatalf("agent feed = %+v, want exactly the Ticket change", f.Changes)
	}
	// The cursor must be past the LAST payroll row, not stuck at the ticket.
	all := feed(t, e, o, 0)
	last := all.Changes[len(all.Changes)-1].Seq
	if f.Cursor != last {
		t.Fatalf("agent cursor = %d, want %d (past the rows it may not see)", f.Cursor, last)
	}
	// Resuming from it yields nothing new.
	if again := feed(t, e, agent, f.Cursor); len(again.Changes) != 0 {
		t.Fatalf("resume returned %+v, want empty", again.Changes)
	}
}

// TestChanges_FilterNarrowsNeverWidens: doctypes=/modules= restrict the feed, and
// asking for a DocType the caller may not read still yields nothing.
func TestChanges_FilterNarrowsNeverWidens(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	o := owner("acme")
	seed(t, e, "acme", ticketDT())
	seed(t, e, "acme", DocType{
		Name: "Article", Module: "cms",
		Fields: []DocField{{Fieldname: "title", Fieldtype: FieldData}},
		Perms:  []DocPerm{{Role: RoleSystemManager, Read: true, Create: true}},
	})
	if _, err := e.CreateDocument(ctx, o, "Ticket", map[string]any{"subject": "t"}); err != nil {
		t.Fatalf("ticket: %v", err)
	}
	if _, err := e.CreateDocument(ctx, o, "Article", map[string]any{"title": "a"}); err != nil {
		t.Fatalf("article: %v", err)
	}

	byDT, err := e.Changes(ctx, o, ChangesQuery{DocTypes: []string{"ticket"}}) // case-insensitive
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(byDT.Changes) != 1 || byDT.Changes[0].DocType != "Ticket" {
		t.Fatalf("doctypes filter = %+v, want one Ticket", byDT.Changes)
	}
	byMod, err := e.Changes(ctx, o, ChangesQuery{Modules: []string{"cms"}})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(byMod.Changes) != 1 || byMod.Changes[0].DocType != "Article" {
		t.Fatalf("modules filter = %+v, want one Article", byMod.Changes)
	}

	// A member with no roles asking for everything gets nothing: the filter
	// cannot grant what the permission calculus withholds.
	if f := feed(t, e, member("acme"), 0); len(f.Changes) != 0 {
		t.Fatalf("role-less member sees %+v, want nothing", f.Changes)
	}
}

// TestChanges_CursorReplaysWithoutGapOrDuplicate: paging with Limit yields each
// change exactly once, in order.
func TestChanges_CursorReplaysWithoutGapOrDuplicate(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	o := owner("acme")
	seed(t, e, "acme", ticketDT())
	const n = 25
	for i := 0; i < n; i++ {
		if _, err := e.CreateDocument(ctx, o, "Ticket", map[string]any{"subject": "t"}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	seen := map[int64]bool{}
	cursor := int64(0)
	for pages := 0; pages < 20; pages++ {
		f, err := e.Changes(ctx, o, ChangesQuery{Since: cursor, Limit: 7})
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		for _, c := range f.Changes {
			if seen[c.Seq] {
				t.Fatalf("duplicate seq %d", c.Seq)
			}
			seen[c.Seq] = true
		}
		if f.Cursor <= cursor && len(f.Changes) == 0 {
			break
		}
		cursor = f.Cursor
	}
	if len(seen) != n {
		t.Fatalf("saw %d changes across pages, want %d", len(seen), n)
	}
}

// TestChanges_CursorFromNow: ChangeCursor is "everything after this", so a client
// that rendered current state from a list call sees each later change once.
func TestChanges_CursorFromNow(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	o := owner("acme")
	seed(t, e, "acme", ticketDT())
	if _, err := e.CreateDocument(ctx, o, "Ticket", map[string]any{"subject": "before"}); err != nil {
		t.Fatalf("before: %v", err)
	}
	cursor, err := e.ChangeCursor(ctx, o)
	if err != nil {
		t.Fatalf("ChangeCursor: %v", err)
	}
	if f := feed(t, e, o, cursor); len(f.Changes) != 0 {
		t.Fatalf("feed from now = %+v, want empty", f.Changes)
	}
	if _, err := e.CreateDocument(ctx, o, "Ticket", map[string]any{"subject": "after"}); err != nil {
		t.Fatalf("after: %v", err)
	}
	f := feed(t, e, o, cursor)
	if len(f.Changes) != 1 {
		t.Fatalf("feed after one write = %+v, want one", f.Changes)
	}
}

// TestChanges_ResetWhenCursorFallsBehindRetention: a client gone longer than the
// retention window is TOLD, not silently handed a gap. And a fresh client with
// cursor 0 is never told to reset.
func TestChanges_ResetWhenCursorFallsBehindRetention(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	o := owner("acme")
	seed(t, e, "acme", ticketDT())

	// TWO rows before the cursor's successor, so trimming them leaves a real gap:
	// losing only the row AT the cursor would lose nothing a resume needed.
	if _, err := e.CreateDocument(ctx, o, "Ticket", map[string]any{"subject": "old-1"}); err != nil {
		t.Fatalf("old-1: %v", err)
	}
	stale := feed(t, e, o, 0).Cursor
	if _, err := e.CreateDocument(ctx, o, "Ticket", map[string]any{"subject": "old-2"}); err != nil {
		t.Fatalf("old-2: %v", err)
	}

	// Age the existing rows past retention, then force a trim on the next write.
	if _, err := e.store.db.ExecContext(ctx, `UPDATE fw_changes SET at = at - 100000`); err != nil {
		t.Fatalf("age rows: %v", err)
	}
	defer func(r time.Duration) { Retention = r }(Retention)
	Retention = time.Hour
	e.store.watchMu.Lock()
	e.store.lastTrim = time.Time{} // due now
	e.store.watchMu.Unlock()

	if _, err := e.CreateDocument(ctx, o, "Ticket", map[string]any{"subject": "new"}); err != nil {
		t.Fatalf("new: %v", err)
	}

	f := feed(t, e, o, stale)
	if !f.Reset {
		t.Fatalf("Reset = false; a cursor behind the retained window must be told to refetch (feed %+v)", f)
	}
	if len(f.Changes) != 1 {
		t.Fatalf("post-reset page = %+v, want the one surviving change", f.Changes)
	}
	if fresh := feed(t, e, o, 0); fresh.Reset {
		t.Fatal("a fresh client (cursor 0) must never be told to reset")
	}
}

// TestChanges_FailedWriteAppendsNothing: the change row and the state change are
// one transaction, so a rejected write leaves no trace in the feed.
func TestChanges_FailedWriteAppendsNothing(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	o := owner("acme")
	dt := ticketDT()
	dt.Autoname = "field:subject" // name is the subject ⇒ a repeat collides
	seed(t, e, "acme", dt)

	if _, err := e.CreateDocument(ctx, o, "Ticket", map[string]any{"subject": "dup"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	before := feed(t, e, o, 0)
	if _, err := e.CreateDocument(ctx, o, "Ticket", map[string]any{"subject": "dup"}); err == nil {
		t.Fatal("duplicate name must be rejected")
	}
	// Deleting a document that is not there is also a non-event.
	_ = e.DeleteDocument(ctx, o, "Ticket", "nope")

	after := feed(t, e, o, 0)
	if len(after.Changes) != len(before.Changes) {
		t.Fatalf("feed grew from %d to %d on writes that did not land: %+v", len(before.Changes), len(after.Changes), after.Changes)
	}
}

// TestChanges_WatchWakesOnCommit: the in-process wake fires after a commit (so a
// woken reader always finds the row), coalesces, and stops on cancel.
func TestChanges_WatchWakesOnCommit(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	o := owner("acme")
	seed(t, e, "acme", ticketDT())

	ch, stop := e.WatchChanges()
	drain(ch) // the seed's DocType definition is not a document change, but be exact

	if _, err := e.CreateDocument(ctx, o, "Ticket", map[string]any{"subject": "ring"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no wake within 2s of a committed write")
	}
	// Woken means readable: the row is visible to a reader right now.
	if f := feed(t, e, o, 0); len(f.Changes) != 1 {
		t.Fatalf("woken reader saw %+v, want the committed change", f.Changes)
	}

	stop()
	if _, err := e.CreateDocument(ctx, o, "Ticket", map[string]any{"subject": "silent"}); err != nil {
		t.Fatalf("create 2: %v", err)
	}
	select {
	case <-ch:
		t.Fatal("a stopped watcher still received a wake")
	case <-time.After(100 * time.Millisecond):
	}
}

func drain(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// TestChanges_WatchDoesNotBlockWriters: a watcher that never reads must not stall
// a write, and must not queue unboundedly.
func TestChanges_WatchDoesNotBlockWriters(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	o := owner("acme")
	seed(t, e, "acme", ticketDT())

	ch, stop := e.WatchChanges()
	defer stop()
	for i := 0; i < 50; i++ {
		if _, err := e.CreateDocument(ctx, o, "Ticket", map[string]any{"subject": "x"}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	n := 0
	for {
		select {
		case <-ch:
			n++
			continue
		default:
		}
		break
	}
	if n != 1 {
		t.Fatalf("drained %d wakes from a single-slot channel, want 1 (coalesced)", n)
	}
}
