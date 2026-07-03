package framework

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "framework.db"))
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustDocType(t *testing.T, s *Store, org string, dt DocType) DocType {
	t.Helper()
	if err := dt.Validate(); err != nil {
		t.Fatalf("doctype %q invalid: %v", dt.Name, err)
	}
	saved, err := s.CreateDocType(context.Background(), org, dt)
	if err != nil {
		t.Fatalf("CreateDocType %q: %v", dt.Name, err)
	}
	return saved
}

// TestDocTypeValidate covers the schema well-formedness gate.
func TestDocTypeValidate(t *testing.T) {
	cases := []struct {
		name string
		dt   DocType
		ok   bool
	}{
		{"ok", DocType{Name: "Task", Fields: []DocField{{Fieldname: "subject", Fieldtype: FieldData}}}, true},
		{"no name", DocType{Fields: []DocField{{Fieldname: "a", Fieldtype: FieldData}}}, false},
		{"reserved name", DocType{Name: "doctypes", Fields: []DocField{{Fieldname: "a", Fieldtype: FieldData}}}, false},
		{"no fields", DocType{Name: "Empty"}, false},
		{"bad fieldname", DocType{Name: "X", Fields: []DocField{{Fieldname: "Bad Name", Fieldtype: FieldData}}}, false},
		{"unknown fieldtype", DocType{Name: "X", Fields: []DocField{{Fieldname: "a", Fieldtype: "Bogus"}}}, false},
		{"select needs options", DocType{Name: "X", Fields: []DocField{{Fieldname: "a", Fieldtype: FieldSelect}}}, false},
		{"link needs options", DocType{Name: "X", Fields: []DocField{{Fieldname: "a", Fieldtype: FieldLink}}}, false},
		{"dup fieldname", DocType{Name: "X", Fields: []DocField{{Fieldname: "a", Fieldtype: FieldData}, {Fieldname: "a", Fieldtype: FieldInt}}}, false},
		{"autoname unknown field", DocType{Name: "X", Autoname: "field:missing", Fields: []DocField{{Fieldname: "a", Fieldtype: FieldData}}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.dt.Validate()
			if c.ok && err != nil {
				t.Fatalf("want valid, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatalf("want invalid, got nil")
			}
		})
	}
}

// TestFieldTypeValidation asserts every fieldtype coerces or rejects correctly —
// the gate RED must not be able to bypass.
func TestFieldTypeValidation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const org = "acme"

	// A Company to link to.
	mustDocType(t, s, org, DocType{Name: "Company", Fields: []DocField{{Fieldname: "title", Fieldtype: FieldData}}})
	comp, err := s.CreateDocument(ctx, org, ptr(DocType{Name: "Company", Fields: []DocField{{Fieldname: "title", Fieldtype: FieldData}}}), map[string]any{"title": "Acme"}, "")
	if err != nil {
		t.Fatalf("seed company: %v", err)
	}

	dt := mustDocType(t, s, org, DocType{Name: "Widget", Fields: []DocField{
		{Fieldname: "code", Fieldtype: FieldData, Reqd: true},
		{Fieldname: "qty", Fieldtype: FieldInt},
		{Fieldname: "price", Fieldtype: FieldCurrency},
		{Fieldname: "active", Fieldtype: FieldCheck},
		{Fieldname: "due", Fieldtype: FieldDate},
		{Fieldname: "status", Fieldtype: FieldSelect, Options: "Open\nClosed"},
		{Fieldname: "company", Fieldtype: FieldLink, Options: "Company"},
	}})

	good := map[string]any{
		"code": "W1", "qty": float64(5), "price": 9.99, "active": true,
		"due": "2026-07-02", "status": "Open", "company": comp.Name,
	}
	out, err := s.validateDoc(ctx, org, &dt, good, nil, "", false)
	if err != nil {
		t.Fatalf("valid doc rejected: %v", err)
	}
	if out["qty"] != int64(5) || out["active"] != 1 || out["price"] != 9.99 || out["status"] != "Open" {
		t.Fatalf("coercion mismatch: %+v", out)
	}

	bad := []struct {
		name  string
		in    map[string]any
		isRef bool
	}{
		{"missing reqd", map[string]any{"qty": float64(1)}, false},
		{"int not int", map[string]any{"code": "W", "qty": "notint"}, false},
		{"int fractional", map[string]any{"code": "W", "qty": 1.5}, false},
		{"bad select", map[string]any{"code": "W", "status": "Bogus"}, false},
		{"bad date", map[string]any{"code": "W", "due": "07/02/2026"}, false},
		{"text not string", map[string]any{"code": float64(3)}, false},
		{"dangling link", map[string]any{"code": "W", "company": "Company-ghost"}, true},
	}
	for _, b := range bad {
		t.Run(b.name, func(t *testing.T) {
			_, err := s.validateDoc(ctx, org, &dt, b.in, nil, "", false)
			if err == nil {
				t.Fatalf("want rejection, got nil")
			}
			if b.isRef && err != errBadRef {
				t.Fatalf("want errBadRef, got %v", err)
			}
		})
	}
}

// TestPasswordHashedAndRedacted proves a Password is never stored or returned in
// the clear, and is preserved across an update.
func TestPasswordHashedAndRedacted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const org = "acme"
	dt := mustDocType(t, s, org, DocType{Name: "Cred", Autoname: "field:key", Fields: []DocField{
		{Fieldname: "key", Fieldtype: FieldData, Reqd: true},
		{Fieldname: "secret", Fieldtype: FieldPassword},
	}})

	out, err := s.validateDoc(ctx, org, &dt, map[string]any{"key": "k1", "secret": "hunter2"}, nil, "", false)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	stored, _ := out["secret"].(string)
	if stored == "hunter2" || stored == "" || !isHashed(stored) {
		t.Fatalf("secret not hashed: %q", stored)
	}
	if !verifyPassword(stored, "hunter2") || verifyPassword(stored, "wrong") {
		t.Fatalf("argon2 verify broken")
	}

	saved, err := s.CreateDocument(ctx, org, &dt, out, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// wireDoc must redact — never the hash, never plaintext.
	wire := wireDoc(&dt, saved, nil)
	if wire["secret"] != redactedMarker {
		t.Fatalf("secret not redacted on wire: %v", wire["secret"])
	}

	// Update WITHOUT resending the password (redacted marker) preserves the hash.
	upd, err := s.validateDoc(ctx, org, &dt, map[string]any{"key": "k1", "secret": redactedMarker}, saved.Data, saved.Name, false)
	if err != nil {
		t.Fatalf("update validate: %v", err)
	}
	if upd["secret"] != stored {
		t.Fatalf("password not preserved on update: got %v want %v", upd["secret"], stored)
	}
}

// TestNaming covers hash, field, prompt, and series naming + counter increment.
func TestNaming(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const org = "acme"

	hashDT := mustDocType(t, s, org, DocType{Name: "H", Fields: []DocField{{Fieldname: "a", Fieldtype: FieldData}}})
	d1, _ := s.CreateDocument(ctx, org, &hashDT, map[string]any{"a": "x"}, "")
	if len(d1.Name) != 32 {
		t.Fatalf("hash name want 32 hex, got %q", d1.Name)
	}

	fieldDT := mustDocType(t, s, org, DocType{Name: "F", Autoname: "field:code", Fields: []DocField{{Fieldname: "code", Fieldtype: FieldData, Reqd: true}}})
	out, _ := s.validateDoc(ctx, org, &fieldDT, map[string]any{"code": "ABC"}, nil, "", false)
	df, _ := s.CreateDocument(ctx, org, &fieldDT, out, "")
	if df.Name != "ABC" {
		t.Fatalf("field naming want ABC, got %q", df.Name)
	}
	// duplicate name → conflict.
	if _, err := s.CreateDocument(ctx, org, &fieldDT, out, ""); err != errConflict {
		t.Fatalf("dup field name want errConflict, got %v", err)
	}

	promptDT := mustDocType(t, s, org, DocType{Name: "P", Autoname: "prompt", Fields: []DocField{{Fieldname: "a", Fieldtype: FieldData}}})
	if _, err := s.CreateDocument(ctx, org, &promptDT, map[string]any{"a": "x"}, ""); err == nil {
		t.Fatalf("prompt naming without name should fail")
	}
	dp, err := s.CreateDocument(ctx, org, &promptDT, map[string]any{"a": "x"}, "MY-001")
	if err != nil || dp.Name != "MY-001" {
		t.Fatalf("prompt naming want MY-001, got %q (%v)", dp.Name, err)
	}

	serDT := mustDocType(t, s, org, DocType{Name: "Inv", Autoname: "INV-.YYYY.-.####", Fields: []DocField{{Fieldname: "a", Fieldtype: FieldData}}})
	s1, _ := s.CreateDocument(ctx, org, &serDT, map[string]any{"a": "x"}, "")
	s2, _ := s.CreateDocument(ctx, org, &serDT, map[string]any{"a": "y"}, "")
	yr := time.Now().Year()
	if !strings.HasPrefix(s1.Name, "INV-") || !strings.HasSuffix(s1.Name, "0001") {
		t.Fatalf("series #1 want INV-<year>-0001, got %q", s1.Name)
	}
	if !strings.HasSuffix(s2.Name, "0002") {
		t.Fatalf("series #2 want …0002, got %q", s2.Name)
	}
	if !strings.Contains(s1.Name, itoa(yr)) {
		t.Fatalf("series name missing year %d: %q", yr, s1.Name)
	}
}

// TestExpandSeries covers the pure pattern parser with a fixed date.
func TestExpandSeries(t *testing.T) {
	now := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	ser := expandSeries("INV-.YYYY.-.#####", now)
	if got := ser.format(7); got != "INV-2026-00007" {
		t.Fatalf("format want INV-2026-00007, got %q", got)
	}
	// No '#' run → default 5-digit counter appended.
	ser2 := expandSeries("TASK-", now)
	if got := ser2.format(3); got != "TASK-00003" {
		t.Fatalf("no-hash want TASK-00003, got %q", got)
	}
}

// TestChildTable validates Table child rows against their child DocType.
func TestChildTable(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const org = "acme"
	mustDocType(t, s, org, DocType{Name: "Line Item", Fields: []DocField{
		{Fieldname: "item", Fieldtype: FieldData, Reqd: true},
		{Fieldname: "qty", Fieldtype: FieldInt},
	}})
	order := mustDocType(t, s, org, DocType{Name: "Order", Fields: []DocField{
		{Fieldname: "customer", Fieldtype: FieldData},
		{Fieldname: "items", Fieldtype: FieldTable, Options: "Line Item"},
	}})

	good := map[string]any{"customer": "Acme", "items": []any{
		map[string]any{"item": "Widget", "qty": float64(2)},
		map[string]any{"item": "Gadget", "qty": float64(1)},
	}}
	out, err := s.validateDoc(ctx, org, &order, good, nil, "", false)
	if err != nil {
		t.Fatalf("valid order rejected: %v", err)
	}
	rows, ok := out["items"].([]map[string]any)
	if !ok || len(rows) != 2 || rows[0]["qty"] != int64(2) {
		t.Fatalf("child rows mismatch: %+v", out["items"])
	}

	// A child row missing a reqd child field → rejected.
	bad := map[string]any{"items": []any{map[string]any{"qty": float64(1)}}}
	if _, err := s.validateDoc(ctx, org, &order, bad, nil, "", false); err == nil {
		t.Fatalf("child missing reqd should be rejected")
	}
}

// TestDocStatusLifecycle covers submit/cancel transitions + guards at the store.
func TestDocStatusLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const org = "acme"
	dt := mustDocType(t, s, org, DocType{Name: "JE", IsSubmittable: true, Fields: []DocField{{Fieldname: "memo", Fieldtype: FieldData}}})
	d, _ := s.CreateDocument(ctx, org, &dt, map[string]any{"memo": "x"}, "")
	if d.DocStatus != 0 {
		t.Fatalf("new doc want docstatus 0, got %d", d.DocStatus)
	}
	// submit 0→1
	sub, err := s.SetDocStatus(ctx, org, "JE", d.Name, 0, 1)
	if err != nil || sub.DocStatus != 1 {
		t.Fatalf("submit want docstatus 1, got %d (%v)", sub.DocStatus, err)
	}
	// double submit → errBadState
	if _, err := s.SetDocStatus(ctx, org, "JE", d.Name, 0, 1); err != errBadState {
		t.Fatalf("double submit want errBadState, got %v", err)
	}
	// edit submitted → errBadState
	if _, err := s.UpdateDocument(ctx, org, &dt, d.Name, map[string]any{"memo": "y"}); err != errBadState {
		t.Fatalf("edit submitted want errBadState, got %v", err)
	}
	// cancel 1→2
	can, err := s.SetDocStatus(ctx, org, "JE", d.Name, 1, 2)
	if err != nil || can.DocStatus != 2 {
		t.Fatalf("cancel want docstatus 2, got %d (%v)", can.DocStatus, err)
	}
}

// TestPerOrgIsolation_Store proves the org column isolates doctypes + documents
// at the store layer (the HTTP layer adds the principal gate on top).
func TestPerOrgIsolation_Store(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	dtA := mustDocType(t, s, "orgA", DocType{Name: "Note", Fields: []DocField{{Fieldname: "body", Fieldtype: FieldData}}})
	docA, _ := s.CreateDocument(ctx, "orgA", &dtA, map[string]any{"body": "secret A"}, "")

	// orgB has no such doctype.
	if _, err := s.GetDocType(ctx, "orgB", "Note"); err != errNotFound {
		t.Fatalf("orgB must not see orgA doctype, got %v", err)
	}
	// orgB cannot read orgA's document even by exact name.
	if _, err := s.GetDocument(ctx, "orgB", "Note", docA.Name); err != errNotFound {
		t.Fatalf("orgB must not read orgA doc, got %v", err)
	}
	// orgB's list of Note is empty.
	rows, _ := s.ListDocuments(ctx, "orgB", "Note", ListOpts{})
	if len(rows) != 0 {
		t.Fatalf("orgB Note list want empty, got %d", len(rows))
	}
	// orgB delete of orgA's doc affects nothing.
	if ok, _ := s.DeleteDocument(ctx, "orgB", "Note", docA.Name); ok {
		t.Fatalf("orgB delete of orgA doc must be a no-op")
	}
	if _, err := s.GetDocument(ctx, "orgA", "Note", docA.Name); err != nil {
		t.Fatalf("orgA doc must survive orgB delete attempt: %v", err)
	}
}

// TestHooks covers the lifecycle interface: before_save mutation persists, an
// on_submit gate aborts, after_save observes.
func TestHooks(t *testing.T) {
	resetHooks()
	t.Cleanup(resetHooks)
	s := testStore(t)
	ctx := context.Background()
	const org = "acme"
	dt := mustDocType(t, s, org, DocType{Name: "Hooked", IsSubmittable: true, Fields: []DocField{
		{Fieldname: "n", Fieldtype: FieldInt},
		{Fieldname: "doubled", Fieldtype: FieldInt},
	}})

	// before_save computes doubled = n*2 (mutation persists).
	RegisterHook("Hooked", ActionBeforeSave, func(_ context.Context, ev *Event) error {
		if n, ok := ev.Doc.Data["n"].(int64); ok {
			ev.Doc.Data["doubled"] = n * 2
		}
		return nil
	})
	// on_submit gate: refuse to submit when n is negative.
	RegisterHook("Hooked", ActionOnSubmit, func(_ context.Context, ev *Event) error {
		if n, _ := ev.Doc.Data["n"].(int64); n < 0 {
			return validationErrorf("n must be non-negative to submit")
		}
		return nil
	})

	out, _ := s.validateDoc(ctx, org, &dt, map[string]any{"n": float64(21)}, nil, "", false)
	doc := Document{DocType: dt.Name, Data: out}
	ev := &Event{Org: org, DocType: dt.Name, Doc: &doc, Meta: &dt, Store: s}
	if err := runHooks(ctx, ActionBeforeSave, ev); err != nil {
		t.Fatalf("before_save: %v", err)
	}
	if doc.Data["doubled"] != int64(42) {
		t.Fatalf("before_save mutation not applied: %v", doc.Data["doubled"])
	}

	// on_submit gate rejects negative.
	neg := Document{DocType: dt.Name, Data: map[string]any{"n": int64(-1)}}
	evNeg := &Event{Org: org, DocType: dt.Name, Doc: &neg, Meta: &dt, Store: s}
	if err := runHooks(ctx, ActionOnSubmit, evNeg); err == nil {
		t.Fatalf("on_submit gate should reject negative n")
	}
}

func ptr(dt DocType) *DocType { return &dt }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
