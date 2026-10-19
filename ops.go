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
	// Grown names the existing DocTypes that gained the fields the module now
	// declares and the org's stored definition lacked.
	Grown []string `json:"grown"`
}

// ---- DocType registry ----

// DefineDocType creates a DocType in the caller's org. Manager-only, and never a
// DocType a module declares: that one is the module's, and an org gets it by
// installing the module (see ours).
func (e *Engine) DefineDocType(ctx context.Context, c Caller, dt DocType) (DocType, error) {
	if err := e.ready(); err != nil {
		return DocType{}, err
	}
	acc, err := e.resolveManager(ctx, c)
	if err != nil {
		return DocType{}, err
	}
	if err := ours(dt.ID()); err != nil {
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
func (e *Engine) DocTypeOf(ctx context.Context, c Caller, id doctype.ID) (DocType, error) {
	if err := e.ready(); err != nil {
		return DocType{}, err
	}
	acc, err := e.resolve(ctx, c)
	if err != nil {
		return DocType{}, err
	}
	return e.store.GetDocType(ctx, acc.Org, id)
}

// ours refuses a definition that belongs to a module rather than to the org.
//
// What a fixture declares — its fields, its permission table, and whether it is
// Immutable — is the subsystem's, which writes each document once through the
// in-process API while every role reads. A replace is the whole definition, so an
// org's own System Manager could drop Immutable and add a write grant and then
// rewrite an act trail; a delete takes the definition and every document under it
// together, which shortens what happened to nothing. Neither is a right over a
// record an insurer and a court read, and no permission row can say so, because a
// row speaks about a role and this is a property of the record.
//
// A DocType the org itself defined is still its own to replace and to remove.
func ours(id doctype.ID) error {
	if declares(id.Module, id.Name) {
		return fmt.Errorf("%w: %s is declared by module %q", ErrForbidden, id, id.Module)
	}
	return nil
}

// ReplaceDocType replaces a DocType definition (PUT semantics). `id` is
// authoritative over the body's own module and name, so a host's URL always wins.
// Documents already stored under it are left intact. Manager-only, and never a
// DocType a module declares.
func (e *Engine) ReplaceDocType(ctx context.Context, c Caller, id doctype.ID, dt DocType) (DocType, error) {
	if err := e.ready(); err != nil {
		return DocType{}, err
	}
	acc, err := e.resolveManager(ctx, c)
	if err != nil {
		return DocType{}, err
	}
	if err := ours(id); err != nil {
		return DocType{}, err
	}
	dt.Module, dt.Name = id.Module, id.Name
	if err := dt.Validate(); err != nil {
		return DocType{}, doctype.Errorf("%s", err.Error())
	}
	return e.store.ReplaceDocType(ctx, acc.Org, dt)
}

// DeleteDocType removes a DocType and all of its documents. Manager-only, and
// never a DocType a module declares.
func (e *Engine) DeleteDocType(ctx context.Context, c Caller, id doctype.ID) error {
	if err := e.ready(); err != nil {
		return err
	}
	acc, err := e.resolveManager(ctx, c)
	if err != nil {
		return err
	}
	if err := ours(id); err != nil {
		return err
	}
	deleted, err := e.store.DeleteDocType(ctx, acc.Org, id)
	if err != nil {
		return err
	}
	if !deleted {
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
		if _, err := e.store.GetDocType(ctx, acc.Org, doctype.ID{Module: module, Name: dt.Name}); err == nil {
			installed = append(installed, dt.Name)
		} else if !errors.Is(err, ErrNotFound) {
			return ModuleState{}, err
		}
	}
	return ModuleState{Module: module, DocTypes: fixtureNames(fx), Installed: installed}, nil
}

// InstallModule ensures every registered DocType of a lane exists in the
// caller's org, with every field the lane declares. Idempotent and manager-only,
// so an owner installs a lane into their OWN tenant and no one else's.
//
// A DocType the org already holds is never replaced. It only GROWS: a field the
// lane declares and the stored definition lacks is appended, and nothing stored
// is removed or altered. That is how a lane that adds a field reaches the orgs
// that installed it earlier — the definition is the module's (an org cannot
// replace it, see ours), so the stored copy is the module's as of its install,
// and without this it would hold that shape for good while the lane's code
// writes fields the store silently drops.
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
	res := Install{Module: module, Created: []string{}, Existing: []string{}, Grown: []string{}}
	for _, dt := range fx {
		dt.Module = module // the lane owns the module tag, and it is half the key
		if held, err := e.store.stored(ctx, acc.Org, dt.ID()); err == nil {
			if more := missing(held, dt); len(more) > 0 {
				held.Fields = append(held.Fields, more...)
				if err := held.Validate(); err != nil {
					return Install{}, fmt.Errorf("fixture %q grown invalid: %w", dt.Name, err)
				}
				if _, err := e.store.ReplaceDocType(ctx, acc.Org, held); err != nil {
					return Install{}, err
				}
				res.Grown = append(res.Grown, dt.Name)
			}
			res.Existing = append(res.Existing, dt.Name)
			continue
		} else if !errors.Is(err, ErrNotFound) {
			return Install{}, err
		}
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

// missing is the fields a fixture declares that a stored definition lacks, by
// fieldname, in the fixture's order. A grown field the fixture requires and gives
// no default is grown optional: every document stored before it has no value for
// it, and requiring one would refuse each of those documents' next update.
func missing(held, fx DocType) []DocField {
	have := make(map[string]bool, len(held.Fields))
	for _, f := range held.Fields {
		have[f.Fieldname] = true
	}
	var out []DocField
	for _, f := range fx.Fields {
		if have[f.Fieldname] {
			continue
		}
		if f.Reqd && f.Default == "" {
			f.Reqd = false
		}
		out = append(out, f)
	}
	return out
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
func (e *Engine) CreateDocument(ctx context.Context, c Caller, id doctype.ID, in map[string]any) (Doc, error) {
	if err := e.ready(); err != nil {
		return Doc{}, err
	}
	acc, dt, err := e.accessDoc(ctx, c, id, doctype.RightCreate)
	if err != nil {
		return Doc{}, err
	}
	if dt.IsSingle {
		return e.writeSingle(ctx, acc, &dt, in)
	}
	if err := fixed(&dt, birth(&dt), in); err != nil {
		return Doc{}, err
	}
	validated, err := e.store.validateDoc(ctx, acc.Org, &dt, in, nil, "", false)
	if err != nil {
		return Doc{}, err
	}
	doc := Document{DocType: dt.ID(), Data: validated}
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
func (e *Engine) ListDocuments(ctx context.Context, c Caller, id doctype.ID, opts ListOpts) ([]Doc, error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	acc, dt, err := e.accessDoc(ctx, c, id, doctype.RightRead)
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
	rows, err := e.store.ListDocuments(ctx, acc.Org, dt.ID(), opts)
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
func (e *Engine) GetDocument(ctx context.Context, c Caller, id doctype.ID, name string) (Doc, error) {
	if err := e.ready(); err != nil {
		return Doc{}, err
	}
	acc, dt, err := e.accessDoc(ctx, c, id, doctype.RightRead)
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
	doc, err := e.store.GetDocument(ctx, acc.Org, dt.ID(), name)
	if err != nil {
		return Doc{}, err
	}
	return Doc{Document: doc, Meta: &dt}, nil
}

// UpdateDocument replaces a DRAFT document's data, running validate →
// before_save → UPDATE → after_save. A submitted or cancelled document is
// immutable, so the submit lifecycle cannot be bypassed by a plain update.
func (e *Engine) UpdateDocument(ctx context.Context, c Caller, id doctype.ID, name string, in map[string]any) (Doc, error) {
	if err := e.ready(); err != nil {
		return Doc{}, err
	}
	acc, dt, err := e.accessDoc(ctx, c, id, doctype.RightWrite)
	if err != nil {
		return Doc{}, err
	}
	if dt.IsSingle {
		return e.writeSingle(ctx, acc, &dt, in)
	}
	prev, err := e.store.GetDocument(ctx, acc.Org, dt.ID(), name)
	if err != nil {
		return Doc{}, err
	}
	if prev.DocStatus != 0 {
		return Doc{}, fmt.Errorf("%w: document is not a draft (docstatus %d); cannot edit", ErrBadState, prev.DocStatus)
	}
	if err := fixed(&dt, carried(&dt, prev.Data), in); err != nil {
		return Doc{}, err
	}
	validated, err := e.store.validateDoc(ctx, acc.Org, &dt, in, prev.Data, name, false)
	if err != nil {
		return Doc{}, err
	}
	doc := Document{DocType: dt.ID(), Name: name, Data: validated, DocStatus: prev.DocStatus}
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
func (e *Engine) DeleteDocument(ctx context.Context, c Caller, id doctype.ID, name string) error {
	if err := e.ready(); err != nil {
		return err
	}
	acc, dt, err := e.accessDoc(ctx, c, id, doctype.RightDelete)
	if err != nil {
		return err
	}
	if dt.IsSingle {
		name = dt.Name
	}
	prev, err := e.store.GetDocument(ctx, acc.Org, dt.ID(), name)
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
	deleted, err := e.store.DeleteDocument(ctx, acc.Org, dt.ID(), name)
	if err != nil {
		return err
	}
	if !deleted {
		return ErrNotFound
	}
	return nil
}

// Submit transitions a submittable document 0→1 after the on_submit gate.
func (e *Engine) Submit(ctx context.Context, c Caller, id doctype.ID, name string) (Doc, error) {
	return e.transition(ctx, c, id, name, doctype.RightSubmit, 0, 1, ActionOnSubmit)
}

// Cancel transitions a submittable document 1→2 after the on_cancel gate.
func (e *Engine) Cancel(ctx context.Context, c Caller, id doctype.ID, name string) (Doc, error) {
	return e.transition(ctx, c, id, name, doctype.RightCancel, 1, 2, ActionOnCancel)
}

// transition is the shared submit/cancel path: gate on the right, require the
// DocType be submittable, run the lifecycle gate hook, then flip docstatus.
func (e *Engine) transition(ctx context.Context, c Caller, id doctype.ID, name, right string, from, to int, action string) (Doc, error) {
	if err := e.ready(); err != nil {
		return Doc{}, err
	}
	acc, dt, err := e.accessDoc(ctx, c, id, right)
	if err != nil {
		return Doc{}, err
	}
	if !dt.IsSubmittable {
		return Doc{}, doctype.Errorf("doctype is not submittable")
	}
	if dt.IsSingle {
		name = dt.Name
	}
	doc, err := e.store.GetDocument(ctx, acc.Org, dt.ID(), name)
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
	saved, err := e.store.SetDocStatus(ctx, acc.Org, dt.ID(), name, from, to)
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
		n, err := e.store.CountDocuments(ctx, acc.Org, dt.ID())
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
	doc, err := e.store.GetDocument(ctx, org, dt.ID(), dt.Name)
	if errors.Is(err, ErrNotFound) {
		return Document{Name: dt.Name, DocType: dt.ID(), Data: map[string]any{}}, nil
	}
	return doc, err
}

// writeSingle upserts the ONE document of a Single DocType (name == doctype
// name), used by both create and update. It enforces the SAME immutability as
// the normal path — a submitted/cancelled Single is not editable, and a
// read-only field is held at what it is already held at — and preserves a
// redacted Password across an unchanged update by passing the current data as
// `prev` to the validator.
func (e *Engine) writeSingle(ctx context.Context, acc Access, dt *DocType, in map[string]any) (Doc, error) {
	cur, curErr := e.store.GetDocument(ctx, acc.Org, dt.ID(), dt.Name)
	var prev map[string]any
	if curErr == nil {
		if cur.DocStatus != 0 {
			return Doc{}, fmt.Errorf("%w: document is not a draft (docstatus %d); cannot edit", ErrBadState, cur.DocStatus)
		}
		prev = cur.Data
	} else if !errors.Is(curErr, ErrNotFound) {
		return Doc{}, curErr
	}
	// A Single is created and updated by the same write, so the hold on its
	// read-only fields is here rather than at either caller: what the DocType
	// declares before the one document exists, and what that document carries
	// once it does.
	was := birth(dt)
	if prev != nil {
		was = carried(dt, prev)
	}
	if err := fixed(dt, was, in); err != nil {
		return Doc{}, err
	}
	validated, err := e.store.validateDoc(ctx, acc.Org, dt, in, prev, dt.Name, false)
	if err != nil {
		return Doc{}, err
	}
	doc := Document{DocType: dt.ID(), Name: dt.Name, Data: validated}
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
	return &Event{Org: org, DocType: dt.ID(), Doc: doc, Prev: prev, Meta: dt, Store: e.store, Logger: e.log}
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

// ---- read-only fields ----
//
// A DocType declares a field read-only when the subsystem that owns the DocType
// maintains it: an incident's state and the stamps its acts leave, the path a
// camera's credential is kept at, a connector's sync cursor. Those values are
// written by the code that earns them, through the in-process API. Write rights
// on a DocType are rights on every document of it and on every field of it, so
// the generic surface is where the distinction has to be made, and this is where
// it is made.

// fixed refuses a body that moves a read-only field off the value it is already
// held at. `was` is that value per field: what the DocType declares before
// anyone writes a document, and what the document carries once one exists. A
// field `was` does not name is not held.
func fixed(dt *DocType, was, in map[string]any) error {
	for _, f := range dt.Fields {
		if !f.ReadOnly {
			continue
		}
		hold, held := was[f.Fieldname]
		sent, named := in[f.Fieldname]
		if !held || !named {
			continue
		}
		// What the value would BECOME, not how it was spelled. A Datetime reads
		// back in the engine's own layout and a console PUTs the whole document
		// back as RFC3339; the same instant in two spellings is one value, and
		// refusing it would make a full-document write impossible on any DocType
		// carrying a read-only field. A value that does not coerce at all is
		// compared as it came and refused by the validator, which says why.
		if val, err := doctype.Coerce(f, sent); err == nil {
			sent = val
		}
		if text(hold) != text(sent) {
			return fmt.Errorf("%w: field %q is read-only", ErrForbidden, f.Fieldname)
		}
	}
	return nil
}

// birth is what a DocType says its read-only fields are before a document
// exists: the defaults it declares. An incident is born `received`, so a create
// that lands it `closed` with stamps of its own is asserting a response that no
// act, no role and no trail produced.
//
// A read-only field the DocType declares nothing for is what the create
// establishes — where a camera's credential is kept is chosen when the camera is
// registered — and after that it is held by the stored value.
//
// So such a field is the CREATE's to choose, and a DocType that also makes it
// Unique has made it first-come: the first create to name a value owns it for
// the org, and every later one is refused. That is correct where the subsystem
// supplies the value and a register to squat where the caller does, which is the
// question to ask of every ReadOnly field carrying no default.
func birth(dt *DocType) map[string]any {
	out := make(map[string]any, len(dt.Fields))
	for _, f := range dt.Fields {
		if f.ReadOnly && f.Default != "" {
			out[f.Fieldname] = f.Default
		}
	}
	return out
}

// carried is what a stored document holds in its read-only fields — every one of
// them, including the ones never written, so an update establishes none of them
// either.
func carried(dt *DocType, prev map[string]any) map[string]any {
	out := make(map[string]any, len(dt.Fields))
	for _, f := range dt.Fields {
		if f.ReadOnly {
			out[f.Fieldname] = prev[f.Fieldname]
		}
	}
	return out
}

// text renders a field value as what it compares by. A stored value arrives
// coerced and a body's does not, an absent field reads as empty, and no
// read-only field is a structure.
func text(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
