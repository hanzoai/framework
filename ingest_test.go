package framework

import (
	"context"
	"testing"

	"github.com/hanzoai/cloud"
	luxlog "github.com/luxfi/log"
	"github.com/zap-proto/zip"
)

// ingest_test.go proves the in-process document-create API (used by KB's connector
// sync) reuses the SAME validate + hook pipeline as the HTTP path, and stays
// physically org-scoped.

// mountForIngest mounts the framework (setting the package-global `mounted`) and
// seeds a doctype in an org via the HTTP install-less direct path (CreateDocType on
// the mounted store) so Ingest has a schema to write against.
func mountForIngest(t *testing.T) (*cloud.Service[state], func()) {
	t.Helper()
	app := zip.New(zip.Config{Logger: luxlog.New("test")})
	if err := Mount(app, cloud.Deps{Logger: luxlog.New("test"), DataDir: t.TempDir()}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	return mounted, func() { _ = Shutdown() }
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
	if _, err := s.State.store.CreateDocType(context.Background(), "acme", dt); err != nil {
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

	ing, err := Ingest(context.Background(), "acme", "note", map[string]any{
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
	doc, err := s.State.store.GetDocument(context.Background(), "acme", "note", ing.Name)
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if doc.Data["title"] != "hello" {
		t.Errorf("persisted doc wrong: %+v", doc.Data)
	}

	// Isolation: the doc must NOT be visible in another org.
	if _, err := s.State.store.GetDocument(context.Background(), "other", "note", ing.Name); err == nil {
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
	if _, err := s.State.store.CreateDocType(context.Background(), "acme", dt); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Missing the required `title` → validation error (same gate as HTTP create).
	if _, err := Ingest(context.Background(), "acme", "note", map[string]any{"body": "x"}, ""); err == nil {
		t.Error("Ingest must reject a document missing a required field")
	}
}

func TestIngestUnknownDocTypeErrors(t *testing.T) {
	_, done := mountForIngest(t)
	defer done()
	if _, err := Ingest(context.Background(), "acme", "does-not-exist", map[string]any{}, ""); err == nil {
		t.Error("Ingest must error on an unknown doctype")
	}
}
