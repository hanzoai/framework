package framework

// presence_test.go proves presence is a fact in the shared store, announced on
// the ONE feed, and confined to a tenant and to what a caller may read.

import (
	"context"
	"testing"
	"time"
)

func TestPresence_JoinRefreshDepart(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	o := owner("acme")
	seed(t, e, "acme", ticketDT())
	doc, err := e.CreateDocument(ctx, o, "Ticket", map[string]any{"subject": "queue"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	cursor := feed(t, e, o, 0).Cursor

	joined, err := e.Announce(ctx, o, "Ticket", doc.Name)
	if err != nil {
		t.Fatalf("Announce: %v", err)
	}
	if !joined {
		t.Fatal("first Announce must report a join")
	}
	room, err := e.Present(ctx, o, "Ticket", doc.Name)
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if len(room) != 1 || room[0].User != o.User {
		t.Fatalf("roster = %+v, want [%s]", room, o.User)
	}

	// The join reached the ONE feed, naming the watched document.
	f := feed(t, e, o, cursor)
	if len(f.Changes) != 1 || f.Changes[0].Action != ChangePresent || f.Changes[0].Name != doc.Name {
		t.Fatalf("feed after join = %+v, want one %q on %s", f.Changes, ChangePresent, doc.Name)
	}
	cursor = f.Cursor

	// A refresh is SILENT: a hundred viewers heartbeating must not flood the feed.
	joined, err = e.Announce(ctx, o, "Ticket", doc.Name)
	if err != nil {
		t.Fatalf("re-Announce: %v", err)
	}
	if joined {
		t.Fatal("re-Announce reported a join; a heartbeat must be silent")
	}
	if f := feed(t, e, o, cursor); len(f.Changes) != 0 {
		t.Fatalf("heartbeat appended %+v to the feed, want nothing", f.Changes)
	}

	if err := e.Depart(ctx, o, "Ticket", doc.Name); err != nil {
		t.Fatalf("Depart: %v", err)
	}
	if room, _ := e.Present(ctx, o, "Ticket", doc.Name); len(room) != 0 {
		t.Fatalf("roster after depart = %+v, want empty", room)
	}
	if f := feed(t, e, o, cursor); len(f.Changes) != 1 || f.Changes[0].Action != ChangeAway {
		t.Fatalf("feed after depart = %+v, want one %q", f.Changes, ChangeAway)
	}
	// Departing twice is not an event.
	if err := e.Depart(ctx, o, "Ticket", doc.Name); err != nil {
		t.Fatalf("second Depart: %v", err)
	}
}

// TestPresence_LapsedViewerIsGone: a viewer that crashes without departing drops
// out when its lease expires — no cleanup has to succeed for the answer to be right.
func TestPresence_LapsedViewerIsGone(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	o := owner("acme")
	seed(t, e, "acme", ticketDT())
	doc, err := e.CreateDocument(ctx, o, "Ticket", map[string]any{"subject": "q"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func(d time.Duration) { PresenceTTL = d }(PresenceTTL)
	PresenceTTL = -time.Second // already expired on write

	if _, err := e.Announce(ctx, o, "Ticket", doc.Name); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	room, err := e.Present(ctx, o, "Ticket", doc.Name)
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if len(room) != 0 {
		t.Fatalf("roster = %+v, want empty (the lease had lapsed)", room)
	}
}

// TestPresence_TenantIsolation: two orgs, the SAME DocType name and the SAME
// document name, each with a viewer. Neither roster may contain the other's
// viewer — dropping the org predicate from the presence query fails this.
func TestPresence_TenantIsolation(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	acme, globex := owner("acme"), owner("globex")
	dt := ticketDT()
	dt.Autoname = "field:subject" // both orgs name their document "shared"
	seed(t, e, "acme", dt)
	seed(t, e, "globex", dt)
	for _, c := range []Caller{acme, globex} {
		if _, err := e.CreateDocument(ctx, c, "Ticket", map[string]any{"subject": "shared"}); err != nil {
			t.Fatalf("%s create: %v", c.Org, err)
		}
		if _, err := e.Announce(ctx, c, "Ticket", "shared"); err != nil {
			t.Fatalf("%s announce: %v", c.Org, err)
		}
	}
	for _, c := range []Caller{acme, globex} {
		room, err := e.Present(ctx, c, "Ticket", "shared")
		if err != nil {
			t.Fatalf("%s Present: %v", c.Org, err)
		}
		if len(room) != 1 || room[0].User != c.User {
			t.Fatalf("%s roster = %+v, want only %s — cross-tenant presence leak", c.Org, room, c.User)
		}
	}
	// Nor does the presence CHANGE cross: each org's feed carries one join.
	for _, c := range []Caller{acme, globex} {
		n := 0
		for _, ch := range feed(t, e, c, 0).Changes {
			if ch.Action == ChangePresent {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%s sees %d presence changes, want 1", c.Org, n)
		}
	}
}

// TestPresence_RequiresReadRight: you cannot appear in — or read — a room on a
// DocType you may not read, and an unvalidated caller never gets that far.
func TestPresence_RequiresReadRight(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	o := owner("acme")
	seed(t, e, "acme", DocType{
		Name:   "Payroll Run",
		Fields: []DocField{{Fieldname: "amount", Fieldtype: FieldCurrency}},
		Perms:  []DocPerm{{Role: RoleSystemManager, Read: true, Write: true, Create: true}},
	})
	if _, err := e.CreateDocument(ctx, o, "Payroll Run", map[string]any{"amount": 1.0}); err != nil {
		t.Fatalf("create: %v", err)
	}
	stranger := member("acme")
	for _, tc := range []struct {
		what string
		err  error
	}{
		{"announce", func() error { _, err := e.Announce(ctx, stranger, "Payroll Run", "x"); return err }()},
		{"present", func() error { _, err := e.Present(ctx, stranger, "Payroll Run", "x"); return err }()},
		{"depart", e.Depart(ctx, stranger, "Payroll Run", "x")},
	} {
		if tc.err == nil {
			t.Fatalf("%s without read right was allowed", tc.what)
		}
		if Classify(tc.err) != CodeForbidden {
			t.Fatalf("%s error = %v (%v), want forbidden", tc.what, tc.err, Classify(tc.err))
		}
	}
	if _, err := e.Announce(ctx, Caller{}, "Payroll Run", "x"); err == nil {
		t.Fatal("Announce with no validated org was allowed")
	}
}

// TestPresence_VisibleOnlyOnReadableDocTypes: a presence change on a DocType the
// caller may not read must not reach that caller's feed — the roster of a hidden
// document is itself hidden.
func TestPresence_VisibleOnlyOnReadableDocTypes(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	o := owner("acme")
	seed(t, e, "acme", ticketDT())
	seed(t, e, "acme", DocType{
		Name:   "Payroll Run",
		Fields: []DocField{{Fieldname: "amount", Fieldtype: FieldCurrency}},
		Perms:  []DocPerm{{Role: RoleSystemManager, Read: true, Write: true, Create: true}},
	})
	agent := member("acme")
	if _, err := e.AssignRole(ctx, o, agent.User, "Agent"); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	pay, err := e.CreateDocument(ctx, o, "Payroll Run", map[string]any{"amount": 1.0})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := e.Announce(ctx, o, "Payroll Run", pay.Name); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	for _, ch := range feed(t, e, agent, 0).Changes {
		if ch.DocType == "Payroll Run" {
			t.Fatalf("agent's feed disclosed presence on a DocType it cannot read: %+v", ch)
		}
	}
}

// TestPresence_SingleNeedsNoName: a Single has exactly one document, so the host
// need not name it.
func TestPresence_SingleNeedsNoName(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	o := owner("acme")
	seed(t, e, "acme", DocType{
		Name: "Support Settings", IsSingle: true,
		Fields: []DocField{{Fieldname: "sla_hours", Fieldtype: FieldInt}},
		Perms:  []DocPerm{{Role: RoleSystemManager, Read: true, Write: true, Create: true}},
	})
	if _, err := e.Announce(ctx, o, "Support Settings", ""); err != nil {
		t.Fatalf("Announce on a Single: %v", err)
	}
	room, err := e.Present(ctx, o, "Support Settings", "")
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if len(room) != 1 {
		t.Fatalf("roster = %+v, want one viewer", room)
	}
}
