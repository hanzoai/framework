package framework

import (
	"context"
	"errors"
	"testing"
)

// always_on_isolation_test.go is the load-bearing SECURITY proof for MarkAlwaysOn:
// making a module always-on defaults its DocType *definition* on for every org, but it
// must NEVER make one org's document RECORDS visible to another. The schema is shared;
// the data is not. This is the invariant that separates "default-on schema" (safe)
// from "cross-tenant data leak" (a critical bug).
func TestAlwaysOn_RecordsStayOrgIsolated(t *testing.T) {
	// Flyer is registered AND marked always-on, so it resolves for any org.
	withAlwaysOnModule(t, "promo", alwaysOnFixtures())
	s := testStore(t)
	ctx := context.Background()

	// The DocType DEFINITION resolves for BOTH orgs (schema is default-on for everyone).
	dtA, err := s.GetDocType(ctx, "orga", "Flyer")
	if err != nil {
		t.Fatalf("orga must resolve the always-on DocType: %v", err)
	}
	if _, err := s.GetDocType(ctx, "orgb", "Flyer"); err != nil {
		t.Fatalf("orgb must resolve the same always-on DocType: %v", err)
	}

	// org A writes a record under the always-on DocType. It is A's alone — the write is
	// physically scoped to org A (fw_documents.org = 'orga').
	saved, err := s.CreateDocument(ctx, "orga", &dtA, map[string]any{"title": "orgA-only secret"}, "")
	if err != nil {
		t.Fatalf("orga CreateDocument: %v", err)
	}
	if saved.Name == "" {
		t.Fatal("document must be named")
	}

	// org B cannot GET org A's record by name, even though the DocType resolves for B.
	if _, err := s.GetDocument(ctx, "orgb", "Flyer", saved.Name); !errors.Is(err, errNotFound) {
		t.Fatalf("cross-tenant read of org A's record must 404 for org B, got %v", err)
	}
	// org B's listing of the always-on DocType is empty — A's record never appears.
	docsB, err := s.ListDocuments(ctx, "orgb", "Flyer", ListOpts{Limit: 100})
	if err != nil {
		t.Fatalf("orgb ListDocuments: %v", err)
	}
	if len(docsB) != 0 {
		t.Fatalf("org B must see ZERO of org A's always-on records, got %d", len(docsB))
	}
	// A per-org count confirms the physical scoping regardless of the listing path.
	if n, err := s.CountDocuments(ctx, "orgb", "Flyer"); err != nil || n != 0 {
		t.Fatalf("org B record count must be 0, got %d (err %v)", n, err)
	}

	// Isolation is not over-blocking: org A still sees exactly its own record.
	docsA, err := s.ListDocuments(ctx, "orga", "Flyer", ListOpts{Limit: 100})
	if err != nil {
		t.Fatalf("orga ListDocuments: %v", err)
	}
	if len(docsA) != 1 || docsA[0].Data["title"] != "orgA-only secret" {
		t.Fatalf("org A must see exactly its own record, got %+v", docsA)
	}
}
