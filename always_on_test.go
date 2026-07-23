package framework

import (
	"context"
	"errors"
	"testing"
)

// alwaysOnFixtures is a one-DocType lane used to exercise the always-on resolution
// path without importing a real app lane (which would be an import cycle).
func alwaysOnFixtures() []DocType {
	return []DocType{
		{Name: "Flyer", TitleField: "title", Fields: []DocField{
			{Fieldname: "title", Fieldtype: FieldData, Reqd: true},
			{Fieldname: "body", Fieldtype: FieldText},
		}},
	}
}

// withAlwaysOnModule registers a lane AND marks it always-on for the test, resetting
// the process-global registry afterward.
func withAlwaysOnModule(t *testing.T, module string, fx []DocType) {
	t.Helper()
	resetModules()
	RegisterModule(module, fx)
	MarkAlwaysOn(module)
	t.Cleanup(resetModules)
}

// TestAlwaysOn_ResolvesFixtureForFreshOrg: an always-on module's DocType resolves
// (GetDocType, the Installed predicate) for an org that never installed it — the
// fix for "module not installed for org". A non-always-on module's DocType does NOT.
func TestAlwaysOn_ResolvesFixtureForFreshOrg(t *testing.T) {
	withAlwaysOnModule(t, "promo", alwaysOnFixtures())
	// A second lane registered but NOT marked always-on stays opt-in.
	RegisterModule("optin", []DocType{{Name: "Ledger", Fields: []DocField{{Fieldname: "amount", Fieldtype: FieldCurrency}}}})

	s := testStore(t)
	ctx := context.Background()

	// The always-on fixture resolves for a fresh org, stamped with its module.
	dt, err := s.GetDocType(ctx, "acme", "Flyer")
	if err != nil {
		t.Fatalf("always-on Flyer must resolve for a fresh org, got %v", err)
	}
	if dt.Module != "promo" {
		t.Fatalf("resolved fixture must be module-stamped, got %q", dt.Module)
	}
	if _, ok := dt.field("title"); !ok {
		t.Fatal("resolved fixture must carry its declared fields")
	}
	// normalize() seeded the secure-by-default System Manager grant.
	if len(dt.Perms) == 0 {
		t.Fatal("resolved fixture must be normalized (secure-by-default perms seeded)")
	}

	// A NON-always-on module's DocType is NOT resolved — it still requires install.
	if _, err := s.GetDocType(ctx, "acme", "Ledger"); !errors.Is(err, errNotFound) {
		t.Fatalf("opt-in module's DocType must NOT resolve without install, got %v", err)
	}
	// An entirely unknown name is still 404.
	if _, err := s.GetDocType(ctx, "acme", "Nope"); !errors.Is(err, errNotFound) {
		t.Fatalf("unknown DocType want errNotFound, got %v", err)
	}
}

// TestAlwaysOn_StoredDefinitionWins: an org that customizes an always-on DocType
// (stores its own row) always wins — the fixture is only the fallback. Mirrors
// guide.activeCurriculum (org override beats the built-in default).
func TestAlwaysOn_StoredDefinitionWins(t *testing.T) {
	withAlwaysOnModule(t, "promo", alwaysOnFixtures())
	s := testStore(t)
	ctx := context.Background()

	// Customize: store a Flyer with an extra field.
	custom := DocType{Name: "Flyer", Module: "promo", TitleField: "title", Fields: []DocField{
		{Fieldname: "title", Fieldtype: FieldData, Reqd: true},
		{Fieldname: "cta", Fieldtype: FieldData},
	}}
	if _, err := s.CreateDocType(ctx, "acme", custom); err != nil {
		t.Fatalf("CreateDocType (customize): %v", err)
	}

	got, err := s.GetDocType(ctx, "acme", "Flyer")
	if err != nil {
		t.Fatalf("GetDocType after customize: %v", err)
	}
	if _, ok := got.field("cta"); !ok {
		t.Fatal("the STORED (customized) definition must win over the fixture")
	}
	if _, ok := got.field("body"); ok {
		t.Fatal("the fixture must NOT be merged into a stored override")
	}

	// A DIFFERENT org still gets the fixture (isolation: one org's override never
	// leaks into another).
	other, err := s.GetDocType(ctx, "maxpower", "Flyer")
	if err != nil {
		t.Fatalf("other org must still resolve the fixture, got %v", err)
	}
	if _, ok := other.field("body"); !ok {
		t.Fatal("a non-customizing org must still see the fixture, not acme's override")
	}
}

// TestAlwaysOn_ListIncludesFixtures: ListDocTypes unions always-on fixtures so a
// fresh org's listing matches GetDocType (no "resolves but doesn't list" asymmetry).
func TestAlwaysOn_ListIncludesFixtures(t *testing.T) {
	withAlwaysOnModule(t, "promo", alwaysOnFixtures())
	s := testStore(t)
	ctx := context.Background()

	list, err := s.ListDocTypes(ctx, "acme")
	if err != nil {
		t.Fatalf("ListDocTypes: %v", err)
	}
	if !hasDocType(list, "Flyer") {
		t.Fatalf("fresh-org listing must include the always-on fixture, got %v", names(list))
	}

	// After customizing, the listing has exactly ONE Flyer (the stored override,
	// deduped against the fixture) — never a duplicate.
	if _, err := s.CreateDocType(ctx, "acme", DocType{Name: "Flyer", Fields: []DocField{{Fieldname: "title", Fieldtype: FieldData}}}); err != nil {
		t.Fatalf("customize: %v", err)
	}
	list, _ = s.ListDocTypes(ctx, "acme")
	if n := count(list, "Flyer"); n != 1 {
		t.Fatalf("customized listing must have exactly one Flyer (no fixture dupe), got %d", n)
	}
}

// TestAlwaysOn_VirtualDocTypeBacksDocumentWrites: a document CREATES under an
// always-on DocType the org never materialized — the fixture-resolved schema backs
// the write (fw_documents has no FK to fw_doctypes), which is exactly what
// content.Generate → framework.Ingest relies on.
func TestAlwaysOn_VirtualDocTypeBacksDocumentWrites(t *testing.T) {
	withAlwaysOnModule(t, "promo", alwaysOnFixtures())
	s := testStore(t)
	ctx := context.Background()

	dt, err := s.GetDocType(ctx, "acme", "Flyer")
	if err != nil {
		t.Fatalf("resolve virtual DocType: %v", err)
	}
	saved, err := s.CreateDocument(ctx, "acme", &dt, map[string]any{"title": "Launch"}, "")
	if err != nil {
		t.Fatalf("CreateDocument against a virtual always-on DocType: %v", err)
	}
	if saved.Name == "" {
		t.Fatal("document must be named")
	}
	// It reads back under (org, doctype, name).
	got, err := s.GetDocument(ctx, "acme", "Flyer", saved.Name)
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if got.Data["title"] != "Launch" {
		t.Fatalf("round-trip data mismatch: %+v", got.Data)
	}
}

// TestAlwaysOn_Registry: AlwaysOnModules lists exactly the marked modules.
func TestAlwaysOn_Registry(t *testing.T) {
	withAlwaysOnModule(t, "promo", alwaysOnFixtures())
	RegisterModule("optin", alwaysOnFixtures()) // registered, not marked
	got := AlwaysOnModules()
	if len(got) != 1 || got[0] != "promo" {
		t.Fatalf("AlwaysOnModules want [promo], got %v", got)
	}
}

func hasDocType(list []DocType, name string) bool { return count(list, name) > 0 }
func count(list []DocType, name string) int {
	n := 0
	for _, dt := range list {
		if dt.Name == name {
			n++
		}
	}
	return n
}
func names(list []DocType) []string {
	out := make([]string, len(list))
	for i, dt := range list {
		out[i] = dt.Name
	}
	return out
}
