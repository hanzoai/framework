package framework

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/hanzoai/doctype"
)

// upgrade_test.go proves the one-way move from the name-keyed schema to the
// address-keyed one against a database actually written in the old shape.
//
// The rename has to be derived rather than tabulated, and these are the cases
// that make a table impossible: help's DocTypes were prefixed "hd-" while its
// module is "help", so the prefix is not the module; erp's "erp-stock-item" must
// reach "stock-item" and not "item", so the strip is at the first dash and not
// the longest suffix; and cms never prefixed anything, so "Page" must survive
// untouched with no exception written for it.

// oldSchema is the schema as it was: a DocType keyed by name alone, a document
// by (doctype, name), and no module column on a document at all.
const oldSchema = `
CREATE TABLE fw_doctypes (
  org TEXT NOT NULL, name TEXT NOT NULL, module TEXT NOT NULL DEFAULT '',
  is_single INTEGER NOT NULL DEFAULT 0, is_submittable INTEGER NOT NULL DEFAULT 0,
  autoname TEXT NOT NULL DEFAULT '', title_field TEXT NOT NULL DEFAULT '',
  fields TEXT NOT NULL DEFAULT '[]', permissions TEXT NOT NULL DEFAULT '[]',
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  PRIMARY KEY (org, name)
);
CREATE TABLE fw_documents (
  org TEXT NOT NULL, doctype TEXT NOT NULL, name TEXT NOT NULL,
  docstatus INTEGER NOT NULL DEFAULT 0, data TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  PRIMARY KEY (org, doctype, name)
);
CREATE TABLE fw_series (org TEXT NOT NULL, series TEXT NOT NULL, current INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (org, series));
CREATE TABLE fw_locks (org TEXT NOT NULL, lockkey TEXT NOT NULL, holder TEXT NOT NULL, expires_at INTEGER NOT NULL, PRIMARY KEY (org, lockkey));
`

// lanes registers the fixture sets the rename is derived from — the same shapes
// the real lanes declare, with their real modules and their now-bare names.
func lanes(t *testing.T) {
	t.Helper()
	doctype.ResetModules()
	t.Cleanup(doctype.ResetModules)
	RegisterModule("kb", []DocType{
		{Name: "page", Fields: []DocField{{Fieldname: "title", Fieldtype: FieldData}}},
		{Name: "link", Fields: []DocField{{Fieldname: "source", Fieldtype: FieldData}}},
	})
	RegisterModule("help", []DocType{
		{Name: "ticket", Fields: []DocField{{Fieldname: "subject", Fieldtype: FieldData}}},
	})
	RegisterModule("erp", []DocType{
		{Name: "item", Fields: []DocField{{Fieldname: "code", Fieldtype: FieldData}}},
		{Name: "stock-item", Fields: []DocField{{Fieldname: "qty", Fieldtype: FieldFloat}}},
	})
	RegisterModule("cms", []DocType{
		{Name: "Page", Fields: []DocField{{Fieldname: "title", Fieldtype: FieldData}}},
	})
	RegisterModule("marketing", []DocType{
		{Name: "Campaign", Fields: []DocField{{Fieldname: "title", Fieldtype: FieldData}}},
	})
	MarkAlwaysOn("marketing")
}

// oldDatabase writes a database in the old shape and returns its path.
func oldDatabase(t *testing.T, rows func(*sql.DB)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "framework.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("old schema: %v", err)
	}
	rows(db)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

func putOldDocType(t *testing.T, db *sql.DB, org, name, module string, fields []DocField) {
	t.Helper()
	blob, _ := json.Marshal(fields)
	if _, err := db.Exec(`INSERT INTO fw_doctypes (org,name,module,fields,permissions,created_at,updated_at) VALUES (?,?,?,?,'[]',1,1)`,
		org, name, module, string(blob)); err != nil {
		t.Fatalf("insert doctype %q: %v", name, err)
	}
}

func putOldDocument(t *testing.T, db *sql.DB, org, dtName, name string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO fw_documents (org,doctype,name,data,created_at,updated_at) VALUES (?,?,?,'{}',1,1)`,
		org, dtName, name); err != nil {
		t.Fatalf("insert document %q: %v", name, err)
	}
}

func TestUpgradeMovesEveryRowOntoItsAddress(t *testing.T) {
	lanes(t)
	path := oldDatabase(t, func(db *sql.DB) {
		putOldDocType(t, db, "acme", "kb-page", "kb", []DocField{
			// The self-link is stored the old way: a bare name.
			{Fieldname: "parent", Fieldtype: FieldLink, Options: "kb-page"},
		})
		putOldDocType(t, db, "acme", "hd-ticket", "help", nil)     // prefix is not the module
		putOldDocType(t, db, "acme", "erp-stock-item", "erp", nil) // must not reach "item"
		putOldDocType(t, db, "acme", "erp-item", "erp", nil)       //
		putOldDocType(t, db, "acme", "Page", "cms", nil)           // never prefixed
		putOldDocType(t, db, "acme", "my-thing", "custom", nil)    // an org's own; no lane claims it
		putOldDocument(t, db, "acme", "kb-page", "welcome")        //
		putOldDocument(t, db, "acme", "erp-stock-item", "SI-1")    //
		putOldDocument(t, db, "acme", "Page", "landing")           //
		putOldDocument(t, db, "acme", "my-thing", "mine")          //
		putOldDocument(t, db, "acme", "Campaign", "spring")        // always-on: no doctype row at all
		putOldDocument(t, db, "other", "kb-page", "welcome")       // another tenant, same key
	})

	s, err := openStore(path, nil)
	if err != nil {
		t.Fatalf("openStore (upgrade): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	for _, want := range []struct {
		id     doctype.ID
		reason string
	}{
		{doctype.ID{Module: "kb", Name: "page"}, "the module prefix comes off"},
		{doctype.ID{Module: "help", Name: "ticket"}, "the prefix is not the module"},
		{doctype.ID{Module: "erp", Name: "stock-item"}, "the strip is at the FIRST dash, not the longest suffix"},
		{doctype.ID{Module: "erp", Name: "item"}, "a one-token name strips to itself"},
		{doctype.ID{Module: "cms", Name: "Page"}, "a lane that never prefixed needs no exception"},
		{doctype.ID{Module: "custom", Name: "my-thing"}, "no lane claims it, so the owner's name stands"},
	} {
		if _, err := s.GetDocType(ctx, "acme", want.id); err != nil {
			t.Errorf("%s did not survive the upgrade (%s): %v", want.id, want.reason, err)
		}
	}

	// The old spellings are gone. No alias, no dual read.
	for _, gone := range []doctype.ID{
		{Module: "kb", Name: "kb-page"},
		{Module: "help", Name: "hd-ticket"},
		{Module: "erp", Name: "erp-stock-item"},
	} {
		if _, err := s.GetDocType(ctx, "acme", gone); err == nil {
			t.Errorf("%s still resolves — the old name was left behind", gone)
		}
	}

	// Documents followed their DocType, and the always-on one found its module in
	// the fixtures rather than in a row it never had.
	for _, want := range []struct {
		id   doctype.ID
		name string
	}{
		{doctype.ID{Module: "kb", Name: "page"}, "welcome"},
		{doctype.ID{Module: "erp", Name: "stock-item"}, "SI-1"},
		{doctype.ID{Module: "cms", Name: "Page"}, "landing"},
		{doctype.ID{Module: "custom", Name: "my-thing"}, "mine"},
		{doctype.ID{Module: "marketing", Name: "Campaign"}, "spring"},
	} {
		got, err := s.GetDocument(ctx, "acme", want.id, want.name)
		if err != nil {
			t.Errorf("document %s/%s did not survive: %v", want.id, want.name, err)
			continue
		}
		if got.DocType != want.id {
			t.Errorf("document %s carries doctype %s", want.name, got.DocType)
		}
	}

	// The Link target was rewritten to an address, so the schema validates under
	// the rule that now governs it.
	dt, err := s.GetDocType(ctx, "acme", doctype.ID{Module: "kb", Name: "page"})
	if err != nil {
		t.Fatal(err)
	}
	f, ok := dt.Field("parent")
	if !ok {
		t.Fatal("the page's parent field is gone")
	}
	if f.Options != "kb.page" {
		t.Errorf("Link target = %q, want kb.page", f.Options)
	}
	if err := dt.Validate(); err != nil {
		t.Errorf("upgraded doctype no longer validates: %v", err)
	}

	// Tenancy survived: the other org's identically-keyed row is still its own.
	if _, err := s.GetDocument(ctx, "other", doctype.ID{Module: "kb", Name: "page"}, "welcome"); err != nil {
		t.Errorf("the second tenant's row did not survive: %v", err)
	}
}

// TestUpgradeRunsOnce: reopening an upgraded database must not move anything
// again. The signal is the schema itself, so a second run has nothing to read.
func TestUpgradeRunsOnce(t *testing.T) {
	lanes(t)
	path := oldDatabase(t, func(db *sql.DB) {
		putOldDocType(t, db, "acme", "kb-page", "kb", nil)
		putOldDocument(t, db, "acme", "kb-page", "welcome")
	})
	ctx := context.Background()
	id := doctype.ID{Module: "kb", Name: "page"}

	for i := range 3 {
		s, err := openStore(path, nil)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if _, err := s.GetDocument(ctx, "acme", id, "welcome"); err != nil {
			t.Fatalf("open %d: the document is gone: %v", i, err)
		}
		n, err := s.CountDocuments(ctx, "acme", id)
		if err != nil || n != 1 {
			t.Fatalf("open %d: %d documents, want 1 (err %v)", i, n, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestFreshDatabaseIsNotUpgraded: a database with no old table has nothing to
// move, and the upgrade must not invent work — or a first boot pays for a
// migration that has no rows.
func TestFreshDatabaseIsNotUpgraded(t *testing.T) {
	lanes(t)
	s := testStore(t)
	stale, err := s.stale(context.Background())
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if stale {
		t.Fatal("a freshly created database reports the old schema")
	}
}
