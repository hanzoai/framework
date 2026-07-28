package framework

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/hanzoai/doctype"
)

// ops_test.go covers the ENGINE OPERATIONS — the layer that carries permission
// enforcement. Before the engine was extracted these gates lived in the HTTP
// handlers and were only reachable through a router; they are now the engine's
// own responsibility, so they are proven here, at the level every host shares.

func testEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := Open(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// owner is the first caller in an org — trust-on-first-use makes it the System
// Manager the moment it performs a manager operation.
func owner(org string) Caller  { return Caller{Org: org, User: "owner@" + org} }
func member(org string) Caller { return Caller{Org: org, User: "member@" + org} }
func admin() Caller            { return Caller{Org: "any", User: "root", IsAdmin: true} }

func invoiceDT() DocType {
	return DocType{
		Name: "Sales Invoice", IsSubmittable: true, TitleField: "customer",
		Fields: []DocField{
			{Fieldname: "customer", Fieldtype: FieldData, Reqd: true},
			{Fieldname: "total", Fieldtype: FieldCurrency},
			{Fieldname: "secret", Fieldtype: FieldPassword},
		},
		Perms: []DocPerm{
			{Role: RoleSystemManager, Read: true, Write: true, Create: true, Delete: true, Submit: true, Cancel: true},
			{Role: "Clerk", Read: true, Create: true},
		},
	}
}

func seed(t *testing.T, e *Engine, org string, dt DocType) DocType {
	t.Helper()
	saved, err := e.DefineDocType(context.Background(), owner(org), dt)
	if err != nil {
		t.Fatalf("DefineDocType: %v", err)
	}
	return saved
}

// ---- authorization ----

// TestOps_NoTenantRefused: an unauthenticated caller is refused before any store
// access. The engine never guesses an org.
func TestOps_NoTenantRefused(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	none := Caller{User: "nobody"}

	if _, err := e.ListDocTypes(ctx, none); !errors.Is(err, ErrForbidden) {
		t.Errorf("ListDocTypes with no org = %v, want ErrForbidden", err)
	}
	if _, err := e.DefineDocType(ctx, none, invoiceDT()); !errors.Is(err, ErrForbidden) {
		t.Errorf("DefineDocType with no org = %v, want ErrForbidden", err)
	}
	if _, err := e.CreateDocument(ctx, none, "Sales Invoice", nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("CreateDocument with no org = %v, want ErrForbidden", err)
	}
	if Classify(errors.New("x")) == CodeForbidden {
		t.Error("Classify must not report an arbitrary error as forbidden")
	}
}

// TestOps_OwnerSeededOnce is trust-on-first-use: the FIRST caller to perform a
// manager operation in an unowned org becomes its System Manager, and every
// later member has no privilege until granted.
func TestOps_OwnerSeededOnce(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()

	seed(t, e, "acme", invoiceDT())

	// A second, different member is NOT a manager.
	if _, err := e.DefineDocType(ctx, member("acme"), DocType{
		Name: "Other", Fields: []DocField{{Fieldname: "a", Fieldtype: FieldData}},
	}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a later member defined a DocType: %v", err)
	}
	// ...until the owner grants them the role.
	if _, err := e.AssignRole(ctx, owner("acme"), "member@acme", RoleSystemManager); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	if _, err := e.DefineDocType(ctx, member("acme"), DocType{
		Name: "Other", Fields: []DocField{{Fieldname: "a", Fieldtype: FieldData}},
	}); err != nil {
		t.Fatalf("granted member still denied: %v", err)
	}
}

// TestOps_RoleLessMemberDenied: a DocType's declared perms are enforced. A Clerk
// may read and create but never write, delete, submit or cancel.
func TestOps_PermsEnforcedPerRight(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	seed(t, e, "acme", invoiceDT())

	clerk := Caller{Org: "acme", User: "clerk@acme"}
	if _, err := e.AssignRole(ctx, owner("acme"), "clerk@acme", "Clerk"); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	doc, err := e.CreateDocument(ctx, clerk, "Sales Invoice", map[string]any{"customer": "Widgets Ltd"})
	if err != nil {
		t.Fatalf("Clerk create denied: %v", err)
	}
	if _, err := e.GetDocument(ctx, clerk, "Sales Invoice", doc.Name); err != nil {
		t.Fatalf("Clerk read denied: %v", err)
	}
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"write", func() error {
			_, err := e.UpdateDocument(ctx, clerk, "Sales Invoice", doc.Name, map[string]any{"customer": "X"})
			return err
		}},
		{"submit", func() error { _, err := e.Submit(ctx, clerk, "Sales Invoice", doc.Name); return err }},
		{"delete", func() error { return e.DeleteDocument(ctx, clerk, "Sales Invoice", doc.Name) }},
	} {
		if err := tc.call(); !errors.Is(err, ErrForbidden) {
			t.Errorf("Clerk %s = %v, want ErrForbidden", tc.name, err)
		}
	}
}

// TestOps_RoleLessMemberFullyDenied: default-deny for a member with no grant.
func TestOps_RoleLessMemberDenied(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	seed(t, e, "acme", invoiceDT())

	stranger := Caller{Org: "acme", User: "stranger@acme"}
	if _, err := e.CreateDocument(ctx, stranger, "Sales Invoice", map[string]any{"customer": "X"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("role-less member created a document: %v", err)
	}
	if _, err := e.ListDocuments(ctx, stranger, "Sales Invoice", ListOpts{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("role-less member listed documents: %v", err)
	}
}

// TestOps_SuperAdminIsManagerEverywhere: a platform superuser needs no grant.
func TestOps_SuperAdmin(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	root := admin()
	root.Org = "acme"
	if _, err := e.DefineDocType(ctx, root, invoiceDT()); err != nil {
		t.Fatalf("superadmin denied: %v", err)
	}
	if _, err := e.CreateDocument(ctx, root, "Sales Invoice", map[string]any{"customer": "X"}); err != nil {
		t.Fatalf("superadmin document create denied: %v", err)
	}
}

// ---- tenant isolation ----

// TestOps_TenantIsolation: one org's schema and documents are invisible to
// another, through the operation layer (not just the store).
func TestOps_TenantIsolation(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	seed(t, e, "orgA", invoiceDT())

	doc, err := e.CreateDocument(ctx, owner("orgA"), "Sales Invoice", map[string]any{"customer": "Secret Co"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// orgB does not even have the DocType.
	if _, err := e.GetDocument(ctx, owner("orgB"), "Sales Invoice", doc.Name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant read = %v, want ErrNotFound", err)
	}
	dts, err := e.ListDocTypes(ctx, owner("orgB"))
	if err != nil {
		t.Fatalf("ListDocTypes: %v", err)
	}
	for _, dt := range dts {
		if dt.Name == "Sales Invoice" {
			t.Fatal("orgB sees orgA's DocType")
		}
	}
}

// ---- document lifecycle ----

func TestOps_DocumentCRUDAndLifecycle(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	c := owner("acme")
	seed(t, e, "acme", invoiceDT())

	doc, err := e.CreateDocument(ctx, c, "Sales Invoice", map[string]any{"customer": "Widgets Ltd", "total": 10.5})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if doc.Meta == nil || doc.Meta.Name != "Sales Invoice" {
		t.Fatal("returned Doc carries no schema")
	}

	got, err := e.GetDocument(ctx, c, "Sales Invoice", doc.Name)
	if err != nil || got.Data["customer"] != "Widgets Ltd" {
		t.Fatalf("get = %+v, %v", got.Data, err)
	}

	if _, err := e.UpdateDocument(ctx, c, "Sales Invoice", doc.Name, map[string]any{"customer": "Gadgets Ltd"}); err != nil {
		t.Fatalf("update: %v", err)
	}

	// submit → immutable
	sub, err := e.Submit(ctx, c, "Sales Invoice", doc.Name)
	if err != nil || sub.DocStatus != 1 {
		t.Fatalf("submit = %+v, %v", sub, err)
	}
	if _, err := e.UpdateDocument(ctx, c, "Sales Invoice", doc.Name, map[string]any{"customer": "Z"}); !errors.Is(err, ErrBadState) {
		t.Fatalf("submitted document was editable: %v", err)
	}
	if err := e.DeleteDocument(ctx, c, "Sales Invoice", doc.Name); !errors.Is(err, ErrBadState) {
		t.Fatalf("submitted document was deletable: %v", err)
	}
	// double submit refused
	if _, err := e.Submit(ctx, c, "Sales Invoice", doc.Name); !errors.Is(err, ErrBadState) {
		t.Fatalf("double submit = %v, want ErrBadState", err)
	}
	// cancel → deletable
	if _, err := e.Cancel(ctx, c, "Sales Invoice", doc.Name); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := e.DeleteDocument(ctx, c, "Sales Invoice", doc.Name); err != nil {
		t.Fatalf("delete after cancel: %v", err)
	}
	if _, err := e.GetDocument(ctx, c, "Sales Invoice", doc.Name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted document still readable: %v", err)
	}
}

// TestOps_NotSubmittableRefused: submit only applies to a submittable DocType.
func TestOps_NotSubmittable(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	c := owner("acme")
	seed(t, e, "acme", DocType{Name: "Note", Fields: []DocField{{Fieldname: "body", Fieldtype: FieldText}}})
	doc, err := e.CreateDocument(ctx, c, "Note", map[string]any{"body": "hi"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = e.Submit(ctx, c, "Note", doc.Name)
	if Classify(err) != CodeInvalid {
		t.Fatalf("submit on a non-submittable doctype = %v (code %v), want CodeInvalid", err, Classify(err))
	}
}

// TestOps_ValidationRefused: a required field is enforced through the operation.
func TestOps_ValidationRefused(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	seed(t, e, "acme", invoiceDT())
	_, err := e.CreateDocument(ctx, owner("acme"), "Sales Invoice", map[string]any{"total": 1})
	if Classify(err) != CodeInvalid {
		t.Fatalf("missing required field = %v (code %v), want CodeInvalid", err, Classify(err))
	}
}

// ---- Password redaction across the module boundary ----

// TestOps_PasswordNeverLeaves is the security invariant that had to survive the
// split: a Password is hashed on write and Wire returns the marker, never the
// hash and never the plaintext.
func TestOps_PasswordNeverLeaves(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	c := owner("acme")
	seed(t, e, "acme", invoiceDT())

	doc, err := e.CreateDocument(ctx, c, "Sales Invoice",
		map[string]any{"customer": "X", "secret": "hunter2"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	stored, _ := doc.Data["secret"].(string)
	if stored == "hunter2" {
		t.Fatal("plaintext password was stored")
	}
	if !doctype.IsHashed(stored) {
		t.Fatalf("stored password is not an argon2id hash: %q", stored)
	}
	wire := doc.Wire(nil)
	if wire["secret"] != doctype.RedactedMarker {
		t.Fatalf("wire leaked %v, want the redaction marker", wire["secret"])
	}
	if strings.Contains(wire["secret"].(string), "argon2") {
		t.Fatal("wire leaked the hash")
	}

	// An update that echoes the marker back preserves the hash rather than
	// storing the marker as a new password.
	upd, err := e.UpdateDocument(ctx, c, "Sales Invoice", doc.Name,
		map[string]any{"customer": "X", "secret": doctype.RedactedMarker})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Data["secret"] != stored {
		t.Fatal("echoing the redaction marker overwrote the stored hash")
	}
	if !doctype.VerifyPassword(upd.Data["secret"].(string), "hunter2") {
		t.Fatal("original password no longer verifies after a marker round-trip")
	}
}

// TestOps_WireProjectsFields: a field projection still carries the envelope.
func TestOps_WireProjection(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	c := owner("acme")
	seed(t, e, "acme", invoiceDT())
	doc, err := e.CreateDocument(ctx, c, "Sales Invoice", map[string]any{"customer": "X", "total": 3.0})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	w := doc.Wire([]string{"customer"})
	if w["customer"] != "X" {
		t.Fatal("projected field missing")
	}
	if _, ok := w["total"]; ok {
		t.Fatal("unprojected field returned")
	}
	for _, k := range []string{"name", "doctype", "docstatus", "createdAt", "updatedAt"} {
		if _, ok := w[k]; !ok {
			t.Fatalf("envelope key %q dropped by projection", k)
		}
	}
}

// ---- Single DocTypes ----

// TestOps_SingleUpsertsAndAlwaysExists: a Single has exactly one document,
// create and update are the same write, and it reads as an empty draft before
// it is ever written.
func TestOps_Single(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	c := owner("acme")
	seed(t, e, "acme", DocType{
		Name: "Settings", IsSingle: true,
		Fields: []DocField{{Fieldname: "theme", Fieldtype: FieldData}},
	})

	got, err := e.GetDocument(ctx, c, "Settings", "Settings")
	if err != nil {
		t.Fatalf("unwritten Single must read as an empty draft: %v", err)
	}
	if len(got.Data) != 0 {
		t.Fatalf("unwritten Single has data: %v", got.Data)
	}

	if _, err := e.CreateDocument(ctx, c, "Settings", map[string]any{"theme": "dark"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := e.UpdateDocument(ctx, c, "Settings", "Settings", map[string]any{"theme": "light"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	docs, err := e.ListDocuments(ctx, c, "Settings", ListOpts{})
	if err != nil || len(docs) != 1 {
		t.Fatalf("Single lists %d documents, want 1 (%v)", len(docs), err)
	}
	if docs[0].Data["theme"] != "light" {
		t.Fatalf("Single not upserted: %v", docs[0].Data)
	}
}

// ---- DocType registry ops ----

func TestOps_DocTypeRegistry(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	c := owner("acme")
	seed(t, e, "acme", invoiceDT())

	if _, err := e.DefineDocType(ctx, c, invoiceDT()); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate define = %v, want ErrConflict", err)
	}
	if _, err := e.DocTypeOf(ctx, c, "Nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown doctype = %v, want ErrNotFound", err)
	}

	// The name in the operation wins over the body — a host's URL is authoritative.
	replaced, err := e.ReplaceDocType(ctx, c, "Sales Invoice", DocType{
		Name: "Ignored", TitleField: "customer",
		Fields: []DocField{{Fieldname: "customer", Fieldtype: FieldData, Reqd: true}},
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if replaced.Name != "Sales Invoice" {
		t.Fatalf("replace honoured the body name: %q", replaced.Name)
	}

	if err := e.DeleteDocType(ctx, c, "Sales Invoice"); err != nil {
		t.Fatalf("delete doctype: %v", err)
	}
	if err := e.DeleteDocType(ctx, c, "Sales Invoice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

// TestOps_InvalidSchemaRefused: a malformed DocType is refused at define time.
func TestOps_InvalidSchemaRefused(t *testing.T) {
	e := testEngine(t)
	_, err := e.DefineDocType(context.Background(), owner("acme"), DocType{Name: "doctypes",
		Fields: []DocField{{Fieldname: "a", Fieldtype: FieldData}}})
	if Classify(err) != CodeInvalid {
		t.Fatalf("reserved name = %v (code %v), want CodeInvalid", err, Classify(err))
	}
}

// ---- roles ----

func TestOps_Roles(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	c := owner("acme")
	seed(t, e, "acme", invoiceDT()) // seeds the owner

	if _, err := e.AssignRole(ctx, c, "  ", "Clerk"); Classify(err) != CodeInvalid {
		t.Fatalf("empty user = %v, want CodeInvalid", err)
	}
	if _, err := e.AssignRole(ctx, c, strings.Repeat("u", doctype.MaxNameLen+1), "Clerk"); Classify(err) != CodeInvalid {
		t.Fatalf("over-long user = %v, want CodeInvalid", err)
	}
	if _, err := e.AssignRole(ctx, c, "clerk@acme", "Clerk"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	roles, err := e.ListRoles(ctx, c)
	if err != nil || len(roles) != 2 { // owner + clerk
		t.Fatalf("ListRoles = %v (%d), %v", roles, len(roles), err)
	}
	if err := e.RevokeRole(ctx, c, "clerk@acme", "Clerk"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := e.RevokeRole(ctx, c, "clerk@acme", "Clerk"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke = %v, want ErrNotFound", err)
	}
}

// ---- modules ----

func TestOps_Modules(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	doctype.ResetModules()
	t.Cleanup(doctype.ResetModules)
	RegisterModule("shop", []DocType{
		{Name: "Widget", Fields: []DocField{{Fieldname: "sku", Fieldtype: FieldData}}},
	})

	c := owner("acme")
	mods, err := e.Modules(ctx, c)
	if err != nil || len(mods) != 1 || mods[0].Module != "shop" {
		t.Fatalf("Modules = %v, %v", mods, err)
	}

	st, err := e.ModuleOf(ctx, c, "shop")
	if err != nil {
		t.Fatalf("ModuleOf: %v", err)
	}
	if len(st.Installed) != 0 {
		t.Fatalf("nothing installed yet, got %v", st.Installed)
	}
	if _, err := e.ModuleOf(ctx, c, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown module = %v, want ErrNotFound", err)
	}

	ins, err := e.InstallModule(ctx, c, "shop")
	if err != nil || len(ins.Created) != 1 {
		t.Fatalf("install = %+v, %v", ins, err)
	}
	// idempotent
	ins2, err := e.InstallModule(ctx, c, "shop")
	if err != nil || len(ins2.Created) != 0 || len(ins2.Existing) != 1 {
		t.Fatalf("re-install not idempotent: %+v, %v", ins2, err)
	}

	// A non-owner member cannot install into someone else's org.
	if _, err := e.InstallModule(ctx, member("acme"), "shop"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-manager installed a module: %v", err)
	}
	// And installing in orgA never touches orgB.
	if _, err := e.DocTypeOf(ctx, owner("orgB"), "Widget"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("module install leaked across tenants: %v", err)
	}
}

// ---- summary ----

func TestOps_Summary(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	c := owner("acme")
	seed(t, e, "acme", invoiceDT())
	for i := 0; i < 3; i++ {
		if _, err := e.CreateDocument(ctx, c, "Sales Invoice", map[string]any{"customer": "X"}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	sum, err := e.Summary(ctx, c)
	if err != nil || sum.DocTypes != 1 || sum.Documents != 3 {
		t.Fatalf("Summary = %+v, %v", sum, err)
	}
	// Another org counts its own nothing.
	if sum, err := e.Summary(ctx, owner("orgB")); err != nil || sum.Documents != 0 {
		t.Fatalf("cross-tenant summary = %+v, %v", sum, err)
	}
}

// ---- hooks through the operation layer ----

// TestOps_GateHookAborts proves a gate hook's veto stops the write and is
// classified as a refusal, not a fault — the contract a host maps to 422.
func TestOps_GateHookAborts(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	c := owner("acme")
	seed(t, e, "acme", invoiceDT())

	resetHooks()
	t.Cleanup(resetHooks)
	RegisterHook("Sales Invoice", ActionBeforeInsert, func(_ context.Context, _ *Event) error {
		return errors.New("nope")
	})

	_, err := e.CreateDocument(ctx, c, "Sales Invoice", map[string]any{"customer": "X"})
	if err == nil {
		t.Fatal("gate hook did not abort the create")
	}
	if Classify(err) != CodeRejected {
		t.Fatalf("hook abort classified %v, want CodeRejected", Classify(err))
	}
	var abort *HookAbort
	if !errors.As(err, &abort) {
		t.Fatalf("hook abort is not a *HookAbort: %T", err)
	}
	docs, err := e.ListDocuments(ctx, c, "Sales Invoice", ListOpts{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(docs) != 0 {
		t.Fatal("aborted create still wrote a document")
	}
}

// TestOps_BeforeSaveMutationPersists: a before_save hook may compute a field.
func TestOps_BeforeSaveMutates(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	c := owner("acme")
	seed(t, e, "acme", invoiceDT())

	resetHooks()
	t.Cleanup(resetHooks)
	RegisterHook("Sales Invoice", ActionBeforeSave, func(_ context.Context, ev *Event) error {
		ev.Doc.Data["total"] = 99.0
		return nil
	})
	doc, err := e.CreateDocument(ctx, c, "Sales Invoice", map[string]any{"customer": "X", "total": 1.0})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if doc.Data["total"] != 99.0 {
		t.Fatalf("before_save mutation not persisted: %v", doc.Data["total"])
	}
}

// ---- list query parsing ----

func TestParseListQuery(t *testing.T) {
	dt := invoiceDT()
	dt.Normalize()

	opts, fields, err := ParseListQuery(&dt, ListQuery{
		Filters: `{"customer":"X"}`, Fields: "customer,total", OrderBy: "customer desc", Limit: "5",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if opts.Filters["customer"] != "X" || opts.OrderField != "customer" || !opts.Desc || opts.Limit != 5 {
		t.Fatalf("parsed = %+v", opts)
	}
	if len(fields) != 2 {
		t.Fatalf("fields = %v", fields)
	}

	// JSON-array field syntax is equivalent to the comma list.
	if _, f, err := ParseListQuery(&dt, ListQuery{Fields: `["customer"]`}); err != nil || len(f) != 1 {
		t.Fatalf("array fields = %v, %v", f, err)
	}
	// Default ordering is most-recently-updated first.
	if o, _, _ := ParseListQuery(&dt, ListQuery{}); !o.Desc || o.Limit != doctype.DefaultLimit {
		t.Fatalf("defaults = %+v", o)
	}
	// The managed columns are queryable.
	if _, _, err := ParseListQuery(&dt, ListQuery{Filters: `{"docstatus":1}`, OrderBy: "name asc"}); err != nil {
		t.Fatalf("managed columns rejected: %v", err)
	}
}

// TestParseListQuery_RejectsUndeclared: a field that is not in the schema never
// reaches a JSON path.
func TestParseListQuery_RejectsUndeclared(t *testing.T) {
	dt := invoiceDT()
	for _, q := range []ListQuery{
		{Filters: `{"nope":"x"}`},
		{Fields: "nope"},
		{OrderBy: "nope asc"},
		{Filters: `not json`},
	} {
		if _, _, err := ParseListQuery(&dt, q); Classify(err) != CodeInvalid {
			t.Errorf("ParseListQuery(%+v) = %v, want CodeInvalid", q, err)
		}
	}
}

func TestBindDocument(t *testing.T) {
	m, err := BindDocument([]byte(`{"a":1}`))
	if err != nil || m["a"] != 1.0 {
		t.Fatalf("bind = %v, %v", m, err)
	}
	if m, err := BindDocument(nil); err != nil || len(m) != 0 {
		t.Fatalf("empty body = %v, %v", m, err)
	}
	if m, err := BindDocument([]byte(`null`)); err != nil || m == nil {
		t.Fatalf("null body must bind to an empty map: %v, %v", m, err)
	}
	if _, err := BindDocument([]byte(`{`)); Classify(err) != CodeInvalid {
		t.Fatalf("malformed JSON = %v, want CodeInvalid", err)
	}
	big := make([]byte, doctype.MaxDocBytes+1)
	if _, err := BindDocument(big); Classify(err) != CodeInvalid {
		t.Fatalf("oversize body = %v, want CodeInvalid", err)
	}
}

// ---- engine lifecycle ----

// TestEngine_ClosedIsHonest: every entry point on a closed engine fails with a
// clear error instead of a nil dereference.
func TestEngine_ClosedIsHonest(t *testing.T) {
	e, err := Open(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
	ctx := context.Background()
	if _, err := e.ListDocTypes(ctx, owner("acme")); err == nil {
		t.Error("closed engine served a read")
	}
	if _, err := e.Ingest(ctx, "acme", "x", nil, ""); err == nil {
		t.Error("closed engine served an ingest")
	}
	if e.Installed(ctx, "acme", "x") {
		t.Error("closed engine reported a doctype installed")
	}
	if _, _, err := e.AcquireLease(ctx, "acme", "k", 0, 0); err == nil {
		t.Error("closed engine granted a lease")
	}
}

func TestEngine_OpenRequiresDir(t *testing.T) {
	if _, err := Open(Config{}); err == nil {
		t.Fatal("Open with no Dir must fail")
	}
}

// TestEngine_InjectedOpenerIsUsed proves the host's storage policy is the one
// that runs — the seam that replaced the hard-coded encrypted opener.
func TestEngine_InjectedOpenerIsUsed(t *testing.T) {
	called := 0
	e, err := Open(Config{Dir: t.TempDir(), OpenDB: func(path string) (*sql.DB, error) {
		called++
		return sql.Open("sqlite", path)
	}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if called != 1 {
		t.Fatalf("injected opener called %d times, want 1", called)
	}
}
