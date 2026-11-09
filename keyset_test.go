package framework

import (
	"context"
	"encoding/base64"
	"testing"
	"time"
)

// TestList_WalksEveryPage keysets through >2500 docs with limit 200: exact
// total, no duplicates, no gaps, and an empty final page.
func TestList_WalksEveryPage(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	c := owner("acme")
	seed(t, e, "acme", invoiceDT())

	const total = 2501
	for i := 0; i < total; i++ {
		if _, err := e.CreateDocument(ctx, c, at("erp", "Sales Invoice"), map[string]any{"customer": "Widgets Ltd", "total": 10.5}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	// Walk every page.
	seen := make(map[string]bool)
	after := After{}
	for page := 0; ; page++ {
		opts := ListOpts{Limit: 200, After: after}
		docs, err := e.ListDocuments(ctx, c, at("erp", "Sales Invoice"), opts)
		if err != nil {
			t.Fatalf("list page %d: %v", page, err)
		}
		if len(docs) == 0 {
			if page == 0 {
				t.Fatalf("first page empty, expected %d docs", total)
			}
			break
		}
		for _, d := range docs {
			if seen[d.Name] {
				t.Fatalf("duplicate name %q on page %d", d.Name, page)
			}
			seen[d.Name] = true
		}
		last := docs[len(docs)-1]
		after = After{UpdatedAt: last.UpdatedAt, Name: last.Name}
		if len(docs) < 200 {
			break // final page
		}
	}

	if len(seen) != total {
		t.Fatalf("saw %d docs, want %d", len(seen), total)
	}
}

// TestList_EmptyList: an empty doctype lists nothing with no error.
func TestList_EmptyList(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	c := owner("acme")
	seed(t, e, "acme", DocType{Name: "Empty", Module: "erp", Fields: []DocField{{Fieldname: "body", Fieldtype: FieldText}}})

	docs, err := e.ListDocuments(ctx, c, at("erp", "Empty"), ListOpts{Limit: 200})
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("empty list returned %d docs, want 0", len(docs))
	}
}

// TestList_CursorStableUnderInserts: inserting docs that sort above the first
// page (newer updated_at) does not shift a page already walked.
func TestList_CursorStableUnderInserts(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	c := owner("acme")
	seed(t, e, "acme", invoiceDT())

	const base = 500
	for i := 0; i < base; i++ {
		if _, err := e.CreateDocument(ctx, c, at("erp", "Sales Invoice"), map[string]any{"customer": "Widgets Ltd", "total": 10.5}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	// Walk page 1 and capture the cursor to page 2.
	first, err := e.ListDocuments(ctx, c, at("erp", "Sales Invoice"), ListOpts{Limit: 200})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first) != 200 {
		t.Fatalf("first page = %d, want 200", len(first))
	}
	last := first[len(first)-1]
	cursor := After{UpdatedAt: last.UpdatedAt, Name: last.Name}

	// Page 2 before any new inserts.
	before, err := e.ListDocuments(ctx, c, at("erp", "Sales Invoice"), ListOpts{Limit: 200, After: cursor})
	if err != nil {
		t.Fatalf("page 2 before: %v", err)
	}
	beforeNames := docNames(before)

	// Insert docs strictly newer (next second) so they sort above page 1.
	time.Sleep(time.Second)
	inserted := make([]string, 300)
	for i := 0; i < 300; i++ {
		d, err := e.CreateDocument(ctx, c, at("erp", "Sales Invoice"), map[string]any{"customer": "Newer Ltd", "total": 1.0})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		inserted[i] = d.Name
	}

	// Page 2 must be unchanged: same docs, none of the inserted ones.
	afterDocs, err := e.ListDocuments(ctx, c, at("erp", "Sales Invoice"), ListOpts{Limit: 200, After: cursor})
	if err != nil {
		t.Fatalf("page 2 after inserts: %v", err)
	}
	afterNames := docNames(afterDocs)
	if len(afterNames) != len(beforeNames) {
		t.Fatalf("page 2 after = %d docs, want %d (cursor shifted)", len(afterNames), len(beforeNames))
	}
	for i, name := range beforeNames {
		if i >= len(afterNames) || afterNames[i] != name {
			t.Fatalf("page 2 changed at index %d: %q vs %q", i, afterNames[i], name)
		}
	}
	insertedSet := make(map[string]bool, len(inserted))
	for _, name := range inserted {
		insertedSet[name] = true
	}
	for _, name := range afterNames {
		if insertedSet[name] {
			t.Fatalf("inserted doc %q leaked onto page 2 after cursor", name)
		}
	}
}

// TestList_OrgIsolation: docs in another org never appear.
func TestList_OrgIsolation(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	ownerA := owner("orgA")
	ownerB := owner("orgB")
	seed(t, e, "orgA", invoiceDT())
	seed(t, e, "orgB", invoiceDT())

	for i := 0; i < 200; i++ {
		if _, err := e.CreateDocument(ctx, ownerA, at("erp", "Sales Invoice"), map[string]any{"customer": "A Ltd", "total": 1.0}); err != nil {
			t.Fatalf("create orgA %d: %v", i, err)
		}
	}
	if _, err := e.CreateDocument(ctx, ownerB, at("erp", "Sales Invoice"), map[string]any{"customer": "B Ltd", "total": 1.0}); err != nil {
		t.Fatalf("create orgB: %v", err)
	}

	docs, err := e.ListDocuments(ctx, ownerA, at("erp", "Sales Invoice"), ListOpts{Limit: 200})
	if err != nil {
		t.Fatalf("list orgA: %v", err)
	}
	for _, d := range docs {
		if d.Data["customer"] == "B Ltd" {
			t.Fatalf("orgB doc %q leaked into orgA's list", d.Name)
		}
	}
	if len(docs) != 200 {
		t.Fatalf("orgA list = %d, want 200", len(docs))
	}
}

// TestParseListQuery_CursorRoundTrip: a cursor encodes and decodes stably.
func TestParseListQuery_CursorRoundTrip(t *testing.T) {
	dt := invoiceDT()
	dt.Normalize()

	opts, _, err := ParseListQuery(&dt, ListQuery{Limit: "200"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts.After = After{UpdatedAt: 1700000000, Name: "INV-00001"}
	cursor := makeCursor("updated_at", true, opts.After)

	got, _, err := ParseListQuery(&dt, ListQuery{Cursor: cursor, Limit: "200"})
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if got.After.UpdatedAt != 1700000000 || got.After.Name != "INV-00001" || !got.Desc || got.Limit != 200 {
		t.Fatalf("round trip = %+v", got.After)
	}
}

// TestParseListQuery_BadCursor: a malformed cursor is a validation error.
func TestParseListQuery_BadCursor(t *testing.T) {
	dt := invoiceDT()
	dt.Normalize()
	for _, raw := range []string{"not-a-cursor", "!!!not-base64!!!", badBase64URL(`{"order":"nope","desc":true}`)} {
		if _, _, err := ParseListQuery(&dt, ListQuery{Cursor: raw}); Classify(err) != CodeInvalid {
			t.Errorf("ParseListQuery(cursor %q) = %v, want CodeInvalid", raw, err)
		}
	}
}

func docNames(docs []Doc) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Name)
	}
	return out
}

// badBase64URL returns raw (no padding) base64url of s.
func badBase64URL(s string) string {
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(s))
}
