package framework

import (
	"context"
	"strings"
	"testing"
)

// held_test.go covers the three rules that decide what a body may say about a
// stored document: a read-only field is held at creation as well as at update,
// an update keeps what the body does not name, and an immutable DocType has no
// writer and no remover at all — not even the org owner, whose System Manager
// role is now read through the same permission table as every other role.

// ledgerDT is a record a machine writes and people read: a state it is born in,
// a stamp its own code lands, a note nobody maintains, and a body field.
func ledgerDT() DocType {
	return DocType{
		Name: "Entry", Module: "ledger", TitleField: "memo",
		Fields: []DocField{
			{Fieldname: "memo", Fieldtype: FieldData},
			{Fieldname: "account", Fieldtype: FieldData},
			{Fieldname: "state", Fieldtype: FieldSelect, Options: "open\nclosed", Default: "open", ReadOnly: true},
			{Fieldname: "closed", Fieldtype: FieldData, ReadOnly: true},
		},
		Perms: []DocPerm{
			{Role: RoleSystemManager, Read: true, Write: true, Create: true, Delete: true},
		},
	}
}

// TestCreateHoldsAReadOnlyDefault: a read-only field the DocType gives a default
// is that value at birth, so a create naming another one is refused.
func TestCreateHoldsAReadOnlyDefault(t *testing.T) {
	e := testEngine(t)
	seed(t, e, "acme", ledgerDT())
	id := at("ledger", "Entry")

	if _, err := e.CreateDocument(context.Background(), owner("acme"), id, map[string]any{
		"memo": "forged", "state": "closed", "closed": "2026-09-16",
	}); err == nil {
		t.Fatal("a create landed a read-only field on a state the DocType does not declare at birth")
	} else if !strings.Contains(err.Error(), "state") {
		t.Fatalf("the refusal must name the field, got %v", err)
	}

	// A read-only field the DocType declares nothing for is the create's to
	// establish, and the default one may be named at the value it already is.
	doc, err := e.CreateDocument(context.Background(), owner("acme"), id, map[string]any{
		"memo": "real", "state": "open", "closed": "2026-09-16",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if doc.Data["closed"] != "2026-09-16" {
		t.Fatalf("the create did not establish the undeclared read-only field: %v", doc.Data)
	}
}

// TestUpdateKeepsWhatTheBodyDoesNotName: a partial body changes what it names
// and nothing else, and an empty value is how a field is cleared.
func TestUpdateKeepsWhatTheBodyDoesNotName(t *testing.T) {
	e := testEngine(t)
	seed(t, e, "acme", ledgerDT())
	id := at("ledger", "Entry")
	made, err := e.CreateDocument(context.Background(), owner("acme"), id, map[string]any{
		"memo": "opening", "account": "1001",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	doc, err := e.UpdateDocument(context.Background(), owner("acme"), id, made.Name, map[string]any{
		"memo": "corrected",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if doc.Data["account"] != "1001" {
		t.Errorf("a body naming only the memo erased the account: %v", doc.Data)
	}
	if doc.Data["memo"] != "corrected" {
		t.Errorf("the body's own field did not change: %v", doc.Data)
	}

	doc, err = e.UpdateDocument(context.Background(), owner("acme"), id, made.Name, map[string]any{
		"memo": "corrected", "account": "",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if doc.Data["account"] != nil && doc.Data["account"] != "" {
		t.Errorf("naming a field empty did not clear it: %v", doc.Data)
	}
}

// TestUpdateHoldsAReadOnlyFieldTheDocumentNeverCarried: a read-only field left
// unwritten is still not the caller's to establish once a document exists.
func TestUpdateHoldsAReadOnlyFieldTheDocumentNeverCarried(t *testing.T) {
	e := testEngine(t)
	seed(t, e, "acme", ledgerDT())
	id := at("ledger", "Entry")
	made, err := e.CreateDocument(context.Background(), owner("acme"), id, map[string]any{"memo": "opening"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := e.UpdateDocument(context.Background(), owner("acme"), id, made.Name, map[string]any{
		"closed": "2026-09-16",
	}); err == nil {
		t.Fatal("an update stamped a read-only field the document never carried")
	}
}

// TestImmutableHasNoWriterAndNoRemover: the record a machine writes is closed to
// every role on the generic surface, and the in-process API — which the owning
// subsystem writes through — still reaches it.
func TestImmutableHasNoWriterAndNoRemover(t *testing.T) {
	e := testEngine(t)
	dt := ledgerDT()
	dt.Immutable = true
	seed(t, e, "acme", dt)
	id := at("ledger", "Entry")

	made, err := e.Ingest(context.Background(), "acme", id, map[string]any{"memo": "act"}, "")
	if err != nil {
		t.Fatalf("the owning subsystem could not write its own record: %v", err)
	}
	if _, err := e.UpdateDocument(context.Background(), owner("acme"), id, made.Name, map[string]any{
		"memo": "rewritten",
	}); err == nil {
		t.Error("the org owner wrote over a document of an immutable DocType")
	}
	if err := e.DeleteDocument(context.Background(), owner("acme"), id, made.Name); err == nil {
		t.Error("the org owner removed a document of an immutable DocType")
	}
	if err := e.DeleteDocument(context.Background(), admin(), id, made.Name); err == nil {
		t.Error("a platform superuser removed a document of an immutable DocType")
	}
	if _, err := e.GetDocument(context.Background(), owner("acme"), id, made.Name); err != nil {
		t.Errorf("an immutable document must still read: %v", err)
	}
}

// TestAPermissionTableWithholdsFromTheOrgOwner: a DocType naming System Manager
// with read alone grants read alone. Manager is the meta-permission that defines
// DocTypes; it is not a document right.
func TestAPermissionTableWithholdsFromTheOrgOwner(t *testing.T) {
	e := testEngine(t)
	dt := ledgerDT()
	dt.Perms = []DocPerm{{Role: RoleSystemManager, Read: true}}
	seed(t, e, "acme", dt)
	id := at("ledger", "Entry")

	if _, err := e.CreateDocument(context.Background(), owner("acme"), id, map[string]any{"memo": "by hand"}); err == nil {
		t.Error("the org owner created a document its DocType grants them no create on")
	}
	made, err := e.Ingest(context.Background(), "acme", id, map[string]any{"memo": "act"}, "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := e.GetDocument(context.Background(), owner("acme"), id, made.Name); err != nil {
		t.Errorf("the read the table does grant was refused: %v", err)
	}
}
