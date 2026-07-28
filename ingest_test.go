package framework

import (
	"context"
	"testing"

	luxlog "github.com/luxfi/log"
)

// ingest_test.go proves the in-process document-create API (used by KB's connector
// sync) reuses the SAME validate + hook pipeline as the HTTP path, and stays
// physically org-scoped.

// mountForIngest opens an Engine and returns it with its closer, so Ingest has a
// live store to write against.
func mountForIngest(t *testing.T) (*Engine, func()) {
	t.Helper()
	e, err := Open(Config{Dir: t.TempDir(), Logger: luxlog.New("test")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return e, func() { _ = e.Close() }
}

func TestIngestValidatesAndFiresHooks(t *testing.T) {
	s, done := mountForIngest(t)
	defer done()

	// Seed a doctype in org "acme".
	dt := DocType{
		Name: "note", Module: "test", TitleField: "title",
		Fields: []DocField{
			{Fieldname: "title", Fieldtype: FieldData, Reqd: true},
			{Fieldname: "body", Fieldtype: FieldText},
		},
		Perms: []DocPerm{{Role: RoleSystemManager, Read: true, Write: true, Create: true, Delete: true}},
	}
	if _, err := s.store.CreateDocType(context.Background(), "acme", dt); err != nil {
		t.Fatalf("seed doctype: %v", err)
	}

	// Register an after_save hook that records what it saw — proves Ingest fires it.
	var gotOrg, gotName, gotTitle string
	fired := 0
	resetHooks()
	RegisterHook("note", ActionAfterSave, func(_ context.Context, ev *Event) error {
		fired++
		gotOrg = ev.Org
		gotName = ev.Doc.Name
		gotTitle, _ = ev.Doc.Data["title"].(string)
		return nil
	})
	t.Cleanup(resetHooks)

	ing, err := s.Ingest(context.Background(), "acme", "note", map[string]any{
		"title": "hello", "body": "world",
	}, "")
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if fired != 1 {
		t.Fatalf("after_save hook fired %d times, want 1", fired)
	}
	if gotOrg != "acme" || gotTitle != "hello" || gotName != ing.Name {
		t.Errorf("hook saw org=%q title=%q name=%q; ingested name=%q", gotOrg, gotTitle, gotName, ing.Name)
	}

	// The document must be persisted and readable in-org.
	doc, err := s.store.GetDocument(context.Background(), "acme", "note", ing.Name)
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if doc.Data["title"] != "hello" {
		t.Errorf("persisted doc wrong: %+v", doc.Data)
	}

	// Isolation: the doc must NOT be visible in another org.
	if _, err := s.store.GetDocument(context.Background(), "other", "note", ing.Name); err == nil {
		t.Error("ingested doc leaked into another org")
	}
}

func TestIngestRejectsMissingRequired(t *testing.T) {
	s, done := mountForIngest(t)
	defer done()
	dt := DocType{
		Name: "note", Module: "test",
		Fields: []DocField{{Fieldname: "title", Fieldtype: FieldData, Reqd: true}},
		Perms:  []DocPerm{{Role: RoleSystemManager, Read: true, Create: true}},
	}
	if _, err := s.store.CreateDocType(context.Background(), "acme", dt); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Missing the required `title` → validation error (same gate as HTTP create).
	if _, err := s.Ingest(context.Background(), "acme", "note", map[string]any{"body": "x"}, ""); err == nil {
		t.Error("Ingest must reject a document missing a required field")
	}
}

func TestIngestUnknownDocTypeErrors(t *testing.T) {
	s, done := mountForIngest(t)
	defer done()
	if _, err := s.Ingest(context.Background(), "acme", "does-not-exist", map[string]any{}, ""); err == nil {
		t.Error("Ingest must error on an unknown doctype")
	}
}

// TestDeleteRunsTrashHookAndRemoves proves the in-process Delete (used by KB edge
// reconciliation) runs the on_trash gate and removes the row, and that a gate error
// aborts the delete — the SAME contract as the HTTP delete path.
func TestDeleteRunsTrashHookAndRemoves(t *testing.T) {
	s, done := mountForIngest(t)
	defer done()
	dt := DocType{
		Name: "note", Module: "test",
		Fields: []DocField{{Fieldname: "title", Fieldtype: FieldData, Reqd: true}},
		Perms:  []DocPerm{{Role: RoleSystemManager, Read: true, Write: true, Create: true, Delete: true}},
	}
	if _, err := s.store.CreateDocType(context.Background(), "acme", dt); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ing, err := s.Ingest(context.Background(), "acme", "note", map[string]any{"title": "x"}, "")
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// on_trash runs and the row is removed.
	fired := 0
	resetHooks()
	RegisterHook("note", ActionOnTrash, func(_ context.Context, _ *Event) error { fired++; return nil })
	t.Cleanup(resetHooks)
	if err := s.Delete(context.Background(), "acme", "note", ing.Name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if fired != 1 {
		t.Fatalf("on_trash fired %d times, want 1", fired)
	}
	if _, err := s.store.GetDocument(context.Background(), "acme", "note", ing.Name); err == nil {
		t.Error("document still present after Delete")
	}

	// A missing document is ErrNotFound (idempotent from the caller's view).
	if err := s.Delete(context.Background(), "acme", "note", "nope"); err != ErrNotFound {
		t.Errorf("Delete of missing doc = %v, want ErrNotFound", err)
	}

	// A gate error aborts the delete.
	ing2, _ := s.Ingest(context.Background(), "acme", "note", map[string]any{"title": "y"}, "")
	resetHooks()
	RegisterHook("note", ActionOnTrash, func(_ context.Context, _ *Event) error { return context.Canceled })
	if err := s.Delete(context.Background(), "acme", "note", ing2.Name); err == nil {
		t.Error("Delete must surface an on_trash gate error")
	}
	if _, err := s.store.GetDocument(context.Background(), "acme", "note", ing2.Name); err != nil {
		t.Error("document should remain after an aborted delete")
	}
}
