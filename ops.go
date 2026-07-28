package framework

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hanzoai/doctype"
)

// ops.go is the engine's OPERATION set — the complete DocType surface, with no
// transport in sight.
//
// Every operation takes a Caller the host already authenticated and enforces
// permissions itself, so authorization cannot be forgotten by a host and does
// not have to be re-implemented by each one. A host's job shrinks to: decode a
// request, resolve a Caller, call an operation, render the result, map the
// error Code. Hanzo Cloud does exactly that over HTTP; a CLI or a job runner
// does the same over its own transport.
//
// Lifecycle hooks run OUTSIDE the store's write transaction (the store holds a
// single SQLite connection, so a hook touching ev.Store inside an open
// transaction would deadlock). Gate hooks therefore run BEFORE the state
// change: an error aborts the operation before anything is written.

// Doc is a document together with the schema it was validated against.
//
// The pair travels together because every caller that renders a document needs
// both, and because Wire — the redaction choke point — must never be handed a
// mismatched schema. Returning a bare Document would let a host marshal it
// directly and leak the argon2 hash of a Password field.
type Doc struct {
	Document
	Meta *doctype.DocType
}

// Summary is the org's DocType and document counts.
type Summary struct {
	DocTypes  int `json:"doctypes"`
	Documents int `json:"documents"`
}

// ModuleInfo is a registered app lane and the DocTypes it installs.
type ModuleInfo struct {
	Module   string   `json:"module"`
	DocTypes []string `json:"doctypes"`
}

// ModuleState is one lane's fixtures plus which already exist in the org — the
// honest install state a UI shows ("set up" vs "installed").
type ModuleState struct {
	Module    string   `json:"module"`
	DocTypes  []string `json:"doctypes"`
	Installed []string `json:"installed"`
}

// Install reports what an install created versus what was already present.
type Install struct {
	Module   string   `json:"module"`
	Created  []string `json:"created"`
	Existing []string `json:"existing"`
}

// ---- DocType registry ----

// DefineDocType creates a DocType in the caller's org. Manager-only.
func (e *Engine) DefineDocType(ctx context.Context, c Caller, dt DocType) (DocType, error) {
	if err := e.ready(); err != nil {
		return DocType{}, err
	}
	acc, err := e.resolveManager(ctx, c)
	if err != nil {
		return DocType{}, err
	}
	if err := dt.Validate(); err != nil {
		return DocType{}, doctype.Errorf("%s", err.Error())
	}
	return e.store.CreateDocType(ctx, acc.Org, dt)
}

// ListDocTypes returns the org's DocTypes, including any always-on fixtures it
// has not customized.
func (e *Engine) ListDocTypes(ctx context.Context, c Caller) ([]DocType, error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	acc, err := e.resolve(ctx, c)
	if err != nil {
		return nil, err
	}
	return e.store.ListDocTypes(ctx, acc.Org)
}

// DocTypeOf returns one DocType definition from the caller's org.
func (e *Engine) DocTypeOf(ctx context.Context, c Caller, name string) (DocType, error) {
	if err := e.ready(); err != nil {
		return DocType{}, err
	}
	acc, err := e.resolve(ctx, c)
	if err != nil {
		return DocType{}, err
	}
	return e.store.GetDocType(ctx, acc.Org, name)
}

// ReplaceDocType replaces a DocType definition (PUT semantics). `name` is
// authoritative over dt.Name, so a host's URL always wins over its body.
// Documents already stored under it are left intact. Manager-only.
func (e *Engine) ReplaceDocType(ctx context.Context, c Caller, name string, dt DocType) (DocType, error) {
	if err := e.ready(); err != nil {
		return DocType{}, err
	}
	acc, err := e.resolveManager(ctx, c)
	if err != nil {
		return DocType{}, err
	}
	dt.Name = name
	if err := dt.Validate(); err != nil {
		return DocType{}, doctype.Errorf("%s", err.Error())
	}
	return e.store.ReplaceDocType(ctx, acc.Org, dt)
}

// DeleteDocType removes a DocType and all of its documents. Manager-only.
func (e *Engine) DeleteDocType(ctx context.Context, c Caller, name string) error {
	if err := e.ready(); err != nil {
		return err
	}
	acc, err := e.resolveManager(ctx, c)
	if err != nil {
		return err
	}
	deleted, err := e.store.DeleteDocType(ctx, acc.Org, name)
	if err != nil {
		return err
	}
	if !deleted {
		return ErrNotFound
	}
	return nil
}

// ---- Roles ----

// ListRoles returns the org's (user, role) assignments.
func (e *Engine) ListRoles(ctx context.Context, c Caller) ([]Role, error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	acc, err := e.resolve(ctx, c)
	if err != nil {
		return nil, err
	}
	return e.store.ListRoles(ctx, acc.Org)
}

// AssignRole grants (user, role) in the caller's org. Manager-only.
func (e *Engine) AssignRole(ctx context.Context, c Caller, user, role string) (Role, error) {
	if err := e.ready(); err != nil {
		return Role{}, err
	}
	acc, err := e.resolveManager(ctx, c)
	if err != nil {
		return Role{}, err
	}
	user, role = strings.TrimSpace(user), strings.TrimSpace(role)
	if user == "" || role == "" {
		return Role{}, doctype.Errorf("user and role are required")
	}
	if len(user) > doctype.MaxNameLen || len(role) > doctype.MaxNameLen {
		return Role{}, doctype.Errorf("user or role too long")
	}
	if err := e.store.AssignRole(ctx, acc.Org, user, role); err != nil {
		return Role{}, err
	}
	return Role{User: user, Role: role}, nil
}

// RevokeRole removes (user, role) in the caller's org. Manager-only.
func (e *Engine) RevokeRole(ctx context.Context, c Caller, user, role string) error {
	if err := e.ready(); err != nil {
		return err
	}
	acc, err := e.resolveManager(ctx, c)
	if err != nil {
		return err
	}
	revoked, err := e.store.RevokeRole(ctx, acc.Org, user, role)
	if err != nil {
		return err
	}
	if !revoked {
		return ErrNotFound
	}
	return nil
}

// ---- Modules (app-lane fixtures) ----

// Modules lists the app lanes compiled into this binary and the DocTypes each
// installs. Any validated member may read the catalog — it is compile-time
// first-party data, not tenant data. Installing is gated separately.
func (e *Engine) Modules(ctx context.Context, c Caller) ([]ModuleInfo, error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	if _, err := e.resolve(ctx, c); err != nil {
		return nil, err
	}
	mods := doctype.RegisteredModules()
	out := make([]ModuleInfo, 0, len(mods))
	for _, m := range mods {
		out = append(out, ModuleInfo{Module: m, DocTypes: fixtureNames(doctype.Fixtures(m))})
	}
	return out, nil
}

// ModuleOf reports one lane's fixtures and which already exist in the org.
func (e *Engine) ModuleOf(ctx context.Context, c Caller, module string) (ModuleState, error) {
	if err := e.ready(); err != nil {
		return ModuleState{}, err
	}
	acc, err := e.resolve(ctx, c)
	if err != nil {
		return ModuleState{}, err
	}
	module = strings.TrimSpace(module)
	fx := doctype.Fixtures(module)
	if len(fx) == 0 {
		return ModuleState{}, fmt.Errorf("%w: unknown module: %s", ErrNotFound, module)
	}
	installed := make([]string, 0, len(fx))
	for _, dt := range fx {
		if _, err := e.store.GetDocType(ctx, acc.Org, dt.Name); err == nil {
			installed = append(installed, dt.Name)
		} else if !errors.Is(err, ErrNotFound) {
			return ModuleState{}, err
		}
	}
	return ModuleState{Module: module, DocTypes: fixtureNames(fx), Installed: installed}, nil
}

// InstallModule ensures every registered DocType of a lane exists in the
// caller's org. Idempotent (create-if-absent — an org's customized DocType is
// NEVER clobbered) and manager-only, so an owner installs a lane into their OWN
// tenant and no one else's.
func (e *Engine) InstallModule(ctx context.Context, c Caller, module string) (Install, error) {
	if err := e.ready(); err != nil {
		return Install{}, err
	}
	acc, err := e.resolveManager(ctx, c)
	if err != nil {
		return Install{}, err
	}
	module = strings.TrimSpace(module)
	fx := doctype.Fixtures(module)
	if len(fx) == 0 {
		return Install{}, fmt.Errorf("%w: unknown module: %s", ErrNotFound, module)
	}
	res := Install{Module: module, Created: []string{}, Existing: []string{}}
	for _, dt := range fx {
		if _, err := e.store.GetDocType(ctx, acc.Org, dt.Name); err == nil {
			res.Existing = append(res.Existing, dt.Name)
			continue
		} else if !errors.Is(err, ErrNotFound) {
			return Install{}, err
		}
		dt.Module = module // the lane owns the module tag
		if err := dt.Validate(); err != nil {
			return Install{}, fmt.Errorf("fixture %q invalid: %w", dt.Name, err)
		}
		if _, err := e.store.CreateDocType(ctx, acc.Org, dt); err != nil {
			if errors.Is(err, ErrConflict) { // lost a race with a concurrent install
				res.Existing = append(res.Existing, dt.Name)
				continue
			}
			return Install{}, err
		}
		res.Created = append(res.Created, dt.Name)
	}
	return res, nil
}

func fixtureNames(fx []DocType) []string {
	names := make([]string, len(fx))
	for i, dt := range fx {
		names[i] = dt.Name
	}
	return names
}

// ---- Documents ----

// CreateDocument validates and inserts a document, running the create hook
// pipeline: validate → before_insert → before_save → INSERT → after_save.
// A Single DocType is upserted instead (create and update are the same write).
func (e *Engine) CreateDocument(ctx context.Context, c Caller, dtName string, in map[string]any) (Doc, error) {
	if err := e.ready(); err != nil {
		return Doc{}, err
	}
	acc, dt, err := e.accessDoc(ctx, c, dtName, doctype.RightCreate)
	if err != nil {
		return Doc{}, err
	}
	if dt.IsSingle {
		return e.writeSingle(ctx, acc, &dt, in)
	}
	validated, err := e.store.validateDoc(ctx, acc.Org, &dt, in, nil, "", false)
	if err != nil {
		return Doc{}, err
	}
	doc := Document{DocType: dt.Name, Data: validated}
	ev := e.event(acc.Org, &dt, &doc, nil)
	if err := e.gate(ctx, ActionBeforeInsert, ev); err != nil {
		return Doc{}, err
	}
	if err := e.gate(ctx, ActionBeforeSave, ev); err != nil {
		return Doc{}, err
	}
	saved, err := e.store.CreateDocument(ctx, acc.Org, &dt, doc.Data, stringField(in, "name"))
	if err != nil {
		return Doc{}, err
	}
	e.after(ctx, acc.Org, &dt, &saved, nil)
	return Doc{Document: saved, Meta: &dt}, nil
}

// ListDocuments returns the org's documents of a DocType. A Single lists as its
// one document (a virtual empty draft when never written).
func (e *Engine) ListDocuments(ctx context.Context, c Caller, dtName string, opts ListOpts) ([]Doc, error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	acc, dt, err := e.accessDoc(ctx, c, dtName, doctype.RightRead)
	if err != nil {
		return nil, err
	}
	if dt.IsSingle {
		doc, err := e.single(ctx, acc.Org, &dt)
		if err != nil {
			return nil, err
		}
		return []Doc{{Document: doc, Meta: &dt}}, nil
	}
	rows, err := e.store.ListDocuments(ctx, acc.Org, dt.Name, opts)
	if err != nil {
		return nil, err
	}
	out := make([]Doc, 0, len(rows))
	for _, d := range rows {
		out = append(out, Doc{Document: d, Meta: &dt})
	}
	return out, nil
}

// GetDocument returns one document by name.
func (e *Engine) GetDocument(ctx context.Context, c Caller, dtName, name string) (Doc, error) {
	if err := e.ready(); err != nil {
		return Doc{}, err
	}
	acc, dt, err := e.accessDoc(ctx, c, dtName, doctype.RightRead)
	if err != nil {
		return Doc{}, err
	}
	if dt.IsSingle {
		doc, err := e.single(ctx, acc.Org, &dt)
		if err != nil {
			return Doc{}, err
		}
		return Doc{Document: doc, Meta: &dt}, nil
	}
	doc, err := e.store.GetDocument(ctx, acc.Org, dt.Name, name)
	if err != nil {
		return Doc{}, err
	}
	return Doc{Document: doc, Meta: &dt}, nil
}

// UpdateDocument replaces a DRAFT document's data, running validate →
// before_save → UPDATE → after_save. A submitted or cancelled document is
// immutable, so the submit lifecycle cannot be bypassed by a plain update.
func (e *Engine) UpdateDocument(ctx context.Context, c Caller, dtName, name string, in map[string]any) (Doc, error) {
	if err := e.ready(); err != nil {
		return Doc{}, err
	}
	acc, dt, err := e.accessDoc(ctx, c, dtName, doctype.RightWrite)
	if err != nil {
		return Doc{}, err
	}
	if dt.IsSingle {
		return e.writeSingle(ctx, acc, &dt, in)
	}
	prev, err := e.store.GetDocument(ctx, acc.Org, dt.Name, name)
	if err != nil {
		return Doc{}, err
	}
	if prev.DocStatus != 0 {
		return Doc{}, fmt.Errorf("%w: document is not a draft (docstatus %d); cannot edit", ErrBadState, prev.DocStatus)
	}
	validated, err := e.store.validateDoc(ctx, acc.Org, &dt, in, prev.Data, name, false)
	if err != nil {
		return Doc{}, err
	}
	doc := Document{DocType: dt.Name, Name: name, Data: validated, DocStatus: prev.DocStatus}
	ev := e.event(acc.Org, &dt, &doc, &prev)
	if err := e.gate(ctx, ActionBeforeSave, ev); err != nil {
		return Doc{}, err
	}
	saved, err := e.store.UpdateDocument(ctx, acc.Org, &dt, name, doc.Data)
	if err != nil {
		return Doc{}, err
	}
	e.after(ctx, acc.Org, &dt, &saved, &prev)
	return Doc{Document: saved, Meta: &dt}, nil
}

// DeleteDocument removes a document after the on_trash gate. A submitted
// document must be cancelled first.
func (e *Engine) DeleteDocument(ctx context.Context, c Caller, dtName, name string) error {
	if err := e.ready(); err != nil {
		return err
	}
	acc, dt, err := e.accessDoc(ctx, c, dtName, doctype.RightDelete)
	if err != nil {
		return err
	}
	if dt.IsSingle {
		name = dt.Name
	}
	prev, err := e.store.GetDocument(ctx, acc.Org, dt.Name, name)
	if err != nil {
		return err
	}
	if prev.DocStatus == 1 {
		return fmt.Errorf("%w: submitted document must be cancelled before deletion", ErrBadState)
	}
	ev := e.event(acc.Org, &dt, &prev, nil)
	if err := e.gate(ctx, ActionOnTrash, ev); err != nil {
		return err
	}
	deleted, err := e.store.DeleteDocument(ctx, acc.Org, dt.Name, name)
	if err != nil {
		return err
	}
	if !deleted {
		return ErrNotFound
	}
	return nil
}

// Submit transitions a submittable document 0→1 after the on_submit gate.
func (e *Engine) Submit(ctx context.Context, c Caller, dtName, name string) (Doc, error) {
	return e.transition(ctx, c, dtName, name, doctype.RightSubmit, 0, 1, ActionOnSubmit)
}

// Cancel transitions a submittable document 1→2 after the on_cancel gate.
func (e *Engine) Cancel(ctx context.Context, c Caller, dtName, name string) (Doc, error) {
	return e.transition(ctx, c, dtName, name, doctype.RightCancel, 1, 2, ActionOnCancel)
}

// transition is the shared submit/cancel path: gate on the right, require the
// DocType be submittable, run the lifecycle gate hook, then flip docstatus.
func (e *Engine) transition(ctx context.Context, c Caller, dtName, name, right string, from, to int, action string) (Doc, error) {
	if err := e.ready(); err != nil {
		return Doc{}, err
	}
	acc, dt, err := e.accessDoc(ctx, c, dtName, right)
	if err != nil {
		return Doc{}, err
	}
	if !dt.IsSubmittable {
		return Doc{}, doctype.Errorf("doctype is not submittable")
	}
	if dt.IsSingle {
		name = dt.Name
	}
	doc, err := e.store.GetDocument(ctx, acc.Org, dt.Name, name)
	if err != nil {
		return Doc{}, err
	}
	if doc.DocStatus != from {
		return Doc{}, fmt.Errorf("%w: illegal docstatus transition from %d", ErrBadState, doc.DocStatus)
	}
	ev := e.event(acc.Org, &dt, &doc, nil)
	if err := e.gate(ctx, action, ev); err != nil {
		return Doc{}, err
	}
	saved, err := e.store.SetDocStatus(ctx, acc.Org, dt.Name, name, from, to)
	if err != nil {
		return Doc{}, err
	}
	return Doc{Document: saved, Meta: &dt}, nil
}

// Summary counts the org's DocTypes and documents.
func (e *Engine) Summary(ctx context.Context, c Caller) (Summary, error) {
	if err := e.ready(); err != nil {
		return Summary{}, err
	}
	acc, err := e.resolve(ctx, c)
	if err != nil {
		return Summary{}, err
	}
	dts, err := e.store.ListDocTypes(ctx, acc.Org)
	if err != nil {
		return Summary{}, err
	}
	var docs int
	for _, dt := range dts {
		n, err := e.store.CountDocuments(ctx, acc.Org, dt.Name)
		if err != nil {
			return Summary{}, err
		}
		docs += n
	}
	return Summary{DocTypes: len(dts), Documents: docs}, nil
}

// ---- shared plumbing ----

// single returns the Single's document, or a virtual empty draft when it has
// not been written yet (a Single always "exists").
func (e *Engine) single(ctx context.Context, org string, dt *DocType) (Document, error) {
	doc, err := e.store.GetDocument(ctx, org, dt.Name, dt.Name)
	if errors.Is(err, ErrNotFound) {
		return Document{Name: dt.Name, DocType: dt.Name, Data: map[string]any{}}, nil
	}
	return doc, err
}

// writeSingle upserts the ONE document of a Single DocType (name == doctype
// name), used by both create and update. It enforces the SAME immutability as
// the normal path — a submitted/cancelled Single is not editable — and
// preserves a redacted Password across an unchanged update by passing the
// current data as `prev` to the validator.
func (e *Engine) writeSingle(ctx context.Context, acc Access, dt *DocType, in map[string]any) (Doc, error) {
	cur, curErr := e.store.GetDocument(ctx, acc.Org, dt.Name, dt.Name)
	var prev map[string]any
	if curErr == nil {
		if cur.DocStatus != 0 {
			return Doc{}, fmt.Errorf("%w: document is not a draft (docstatus %d); cannot edit", ErrBadState, cur.DocStatus)
		}
		prev = cur.Data
	} else if !errors.Is(curErr, ErrNotFound) {
		return Doc{}, curErr
	}
	validated, err := e.store.validateDoc(ctx, acc.Org, dt, in, prev, dt.Name, false)
	if err != nil {
		return Doc{}, err
	}
	doc := Document{DocType: dt.Name, Name: dt.Name, Data: validated}
	ev := e.event(acc.Org, dt, &doc, nil)
	if err := e.gate(ctx, ActionBeforeSave, ev); err != nil {
		return Doc{}, err
	}
	saved, err := e.store.UpsertSingle(ctx, acc.Org, dt, doc.Data)
	if err != nil {
		return Doc{}, err
	}
	e.after(ctx, acc.Org, dt, &saved, nil)
	return Doc{Document: saved, Meta: dt}, nil
}

// event builds the value a lifecycle hook receives.
func (e *Engine) event(org string, dt *DocType, doc, prev *Document) *Event {
	return &Event{Org: org, DocType: dt.Name, Doc: doc, Prev: prev, Meta: dt, Store: e.store, Logger: e.log}
}

// gate runs a GATE phase: a hook error aborts the operation before any state
// change, wrapped so the host can tell a refusal from a fault.
func (e *Engine) gate(ctx context.Context, action string, ev *Event) error {
	if err := runHooks(ctx, action, ev); err != nil {
		return &HookAbort{Err: err}
	}
	return nil
}

// after runs after_save hooks. A failure is logged, not fatal — the write
// already committed, and gates belong in before_save/on_submit.
func (e *Engine) after(ctx context.Context, org string, dt *DocType, doc, prev *Document) {
	ev := e.event(org, dt, doc, prev)
	if err := runHooks(ctx, ActionAfterSave, ev); err != nil {
		e.log.Warn("after_save hook failed", "doctype", dt.Name, "name", doc.Name, "err", err)
	}
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}
