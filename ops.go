package framework

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hanzoai/cloud"
	luxlog "github.com/luxfi/log"
	"github.com/zap-proto/zip"
)

type svc struct {
	store *Store
	log   luxlog.Logger
	brand string
}

// mounted is the active service so Shutdown can release the store.
var mounted *svc

// Mount wires the framework surface onto app per HIP-0106.
func Mount(app *zip.App, deps cloud.Deps) error {
	if app == nil {
		return fmt.Errorf("framework.Mount: nil zip.App")
	}
	log := deps.Logger
	if log == nil {
		return fmt.Errorf("framework.Mount: nil deps.Logger")
	}
	log = log.New("subsystem", "framework")
	if deps.DataDir == "" {
		return fmt.Errorf("framework.Mount: empty DataDir")
	}
	if err := os.MkdirAll(deps.DataDir, 0o755); err != nil {
		return fmt.Errorf("framework.Mount: data dir: %w", err)
	}
	store, err := openStore(filepath.Join(deps.DataDir, "framework.db"))
	if err != nil {
		return fmt.Errorf("framework.Mount: open store: %w", err)
	}
	s := &svc{store: store, log: log, brand: deps.Brand}
	mounted = s

	// STATIC routes register BEFORE the generic /:doctype routes so Fiber's
	// first-match scan resolves them unambiguously; their names are also reserved
	// DocType names (see reservedDocTypeNames), so no document route can shadow them.
	app.Get("/v1/framework/summary", s.summary)

	app.Get("/v1/framework/doctypes", s.listDocTypes)
	app.Post("/v1/framework/doctypes", s.createDocType)
	app.Get("/v1/framework/doctypes/:name", s.getDocType)
	app.Put("/v1/framework/doctypes/:name", s.replaceDocType)
	app.Delete("/v1/framework/doctypes/:name", s.deleteDocType)

	app.Get("/v1/framework/roles", s.listRoles)
	app.Post("/v1/framework/roles", s.assignRole)
	app.Delete("/v1/framework/roles/:user/:role", s.revokeRole)

	// App-lane module fixtures: list the registered lanes, inspect one, and
	// install its DocType fixtures into the caller's org. Registered BEFORE the
	// generic /:doctype routes so "modules" resolves to these static handlers, and
	// "modules" is a reserved DocType name so no document route can shadow them.
	app.Get("/v1/framework/modules", s.listModules)
	app.Get("/v1/framework/modules/:module", s.getModule)
	app.Post("/v1/framework/modules/:module/install", s.installModule)

	// GENERIC metadata-driven document surface.
	app.Get("/v1/framework/:doctype", s.listDocuments)
	app.Post("/v1/framework/:doctype", s.createDocument)
	app.Get("/v1/framework/:doctype/:name", s.getDocument)
	app.Put("/v1/framework/:doctype/:name", s.updateDocument)
	app.Delete("/v1/framework/:doctype/:name", s.deleteDocument)
	app.Post("/v1/framework/:doctype/:name/submit", s.submitDocument)
	app.Post("/v1/framework/:doctype/:name/cancel", s.cancelDocument)

	log.Info("framework mounted", "brand", deps.Brand)
	return nil
}

func init() {
	cloud.RegisterWithShutdown("framework", 129,
		func(app any, deps cloud.Deps) error {
			a, ok := app.(*zip.App)
			if !ok {
				return fmt.Errorf("framework.Mount: app is %T, want *zip.App", app)
			}
			return Mount(a, deps)
		},
		func(ctx context.Context) error { return Shutdown() },
	)
}

// Shutdown closes the framework store. Idempotent.
func Shutdown() error {
	if mounted == nil || mounted.store == nil {
		return nil
	}
	err := mounted.store.Close()
	mounted = nil
	return err
}

// ---- DocType registry handlers ----

func (s *svc) createDocType(c *zip.Ctx) error {
	acc, err := s.managerOnly(c)
	if err != nil {
		return err
	}
	var dt DocType
	if err := c.Bind(&dt); err != nil {
		return err
	}
	if err := dt.Validate(); err != nil {
		return zip.ErrBadRequest(err.Error())
	}
	saved, err := s.store.CreateDocType(c.Context(), acc.org, dt)
	if err != nil {
		return mapErr(err, "")
	}
	return c.JSON(http.StatusCreated, saved)
}

func (s *svc) listDocTypes(c *zip.Ctx) error {
	acc, err := s.resolveAccess(c)
	if err != nil {
		return err
	}
	rows, err := s.store.ListDocTypes(c.Context(), acc.org)
	if err != nil {
		return zip.Errorf(http.StatusInternalServerError, "list doctypes: %v", err)
	}
	return c.JSON(http.StatusOK, map[string]any{"data": rows})
}

func (s *svc) getDocType(c *zip.Ctx) error {
	acc, err := s.resolveAccess(c)
	if err != nil {
		return err
	}
	dt, err := s.store.GetDocType(c.Context(), acc.org, pathParam(c, "name"))
	if err != nil {
		return mapErr(err, "doctype not found")
	}
	return c.JSON(http.StatusOK, dt)
}

func (s *svc) replaceDocType(c *zip.Ctx) error {
	acc, err := s.managerOnly(c)
	if err != nil {
		return err
	}
	var dt DocType
	if err := c.Bind(&dt); err != nil {
		return err
	}
	dt.Name = pathParam(c, "name") // the URL is authoritative (decoded)
	if err := dt.Validate(); err != nil {
		return zip.ErrBadRequest(err.Error())
	}
	saved, err := s.store.ReplaceDocType(c.Context(), acc.org, dt)
	if err != nil {
		return mapErr(err, "doctype not found")
	}
	return c.JSON(http.StatusOK, saved)
}

func (s *svc) deleteDocType(c *zip.Ctx) error {
	acc, err := s.managerOnly(c)
	if err != nil {
		return err
	}
	deleted, err := s.store.DeleteDocType(c.Context(), acc.org, pathParam(c, "name"))
	if err != nil {
		return zip.Errorf(http.StatusInternalServerError, "delete doctype: %v", err)
	}
	if !deleted {
		return zip.ErrNotFound("doctype not found")
	}
	return c.NoContent(http.StatusNoContent)
}

// ---- Role handlers ----

func (s *svc) listRoles(c *zip.Ctx) error {
	acc, err := s.resolveAccess(c)
	if err != nil {
		return err
	}
	rows, err := s.store.ListRoles(c.Context(), acc.org)
	if err != nil {
		return zip.Errorf(http.StatusInternalServerError, "list roles: %v", err)
	}
	return c.JSON(http.StatusOK, map[string]any{"data": rows})
}

func (s *svc) assignRole(c *zip.Ctx) error {
	acc, err := s.managerOnly(c)
	if err != nil {
		return err
	}
	var body Role
	if err := c.Bind(&body); err != nil {
		return err
	}
	user := strings.TrimSpace(body.User)
	role := strings.TrimSpace(body.Role)
	if user == "" || role == "" {
		return zip.ErrBadRequest("user and role are required")
	}
	if len(user) > maxNameLen || len(role) > maxNameLen {
		return zip.ErrBadRequest("user or role too long")
	}
	if err := s.store.AssignRole(c.Context(), acc.org, user, role); err != nil {
		return zip.Errorf(http.StatusInternalServerError, "assign role: %v", err)
	}
	return c.JSON(http.StatusCreated, Role{User: user, Role: role})
}

func (s *svc) revokeRole(c *zip.Ctx) error {
	acc, err := s.managerOnly(c)
	if err != nil {
		return err
	}
	revoked, err := s.store.RevokeRole(c.Context(), acc.org, pathParam(c, "user"), pathParam(c, "role"))
	if err != nil {
		return zip.Errorf(http.StatusInternalServerError, "revoke role: %v", err)
	}
	if !revoked {
		return zip.ErrNotFound("role assignment not found")
	}
	return c.NoContent(http.StatusNoContent)
}

// ---- Module (app-lane fixture) handlers ----

// listModules reports the app lanes registered in this binary and the DocType
// names each installs. Any validated member may read the catalog (it is
// compile-time first-party data, not tenant data); installing is gated separately.
func (s *svc) listModules(c *zip.Ctx) error {
	if _, err := s.resolveAccess(c); err != nil {
		return err
	}
	mods := registeredModules()
	out := make([]map[string]any, 0, len(mods))
	for _, m := range mods {
		out = append(out, map[string]any{"module": m, "doctypes": fixtureNames(moduleFixtures(m))})
	}
	return c.JSON(http.StatusOK, map[string]any{"data": out})
}

// getModule reports one lane's fixtures and which of them are already installed in
// the caller's org — the honest install-state the UI shows ("set up" vs "installed").
func (s *svc) getModule(c *zip.Ctx) error {
	acc, err := s.resolveAccess(c)
	if err != nil {
		return err
	}
	module := strings.TrimSpace(c.Param("module"))
	fx := moduleFixtures(module)
	if len(fx) == 0 {
		return zip.ErrNotFound("unknown module: " + module)
	}
	installed := make([]string, 0, len(fx))
	for _, dt := range fx {
		if _, err := s.store.GetDocType(c.Context(), acc.org, dt.Name); err == nil {
			installed = append(installed, dt.Name)
		} else if !errors.Is(err, errNotFound) {
			return mapErr(err, "")
		}
	}
	return c.JSON(http.StatusOK, map[string]any{"module": module, "doctypes": fixtureNames(fx), "installed": installed})
}

// installModule ensures every registered DocType of a lane exists in the caller's
// org. It is idempotent (create-if-absent — an org's customised DocType is NEVER
// clobbered) and gated by managerOnly, so the org owner (System Manager, seeded
// trust-on-first-use) installs a lane into their OWN tenant and no one else's. The
// module name is stamped onto each fixture so the lane's DocTypes are discoverable
// by `module`.
func (s *svc) installModule(c *zip.Ctx) error {
	acc, err := s.managerOnly(c)
	if err != nil {
		return err
	}
	module := strings.TrimSpace(c.Param("module"))
	fx := moduleFixtures(module)
	if len(fx) == 0 {
		return zip.ErrNotFound("unknown module: " + module)
	}
	created := make([]string, 0, len(fx))
	existing := make([]string, 0, len(fx))
	for _, dt := range fx {
		if _, err := s.store.GetDocType(c.Context(), acc.org, dt.Name); err == nil {
			existing = append(existing, dt.Name)
			continue
		} else if !errors.Is(err, errNotFound) {
			return mapErr(err, "")
		}
		dt.Module = module // the lane owns the module tag
		if err := dt.Validate(); err != nil {
			return zip.Errorf(http.StatusInternalServerError, "fixture %q invalid: %v", dt.Name, err)
		}
		if _, err := s.store.CreateDocType(c.Context(), acc.org, dt); err != nil {
			if errors.Is(err, errConflict) { // lost a race with a concurrent install
				existing = append(existing, dt.Name)
				continue
			}
			return mapErr(err, "")
		}
		created = append(created, dt.Name)
	}
	return c.JSON(http.StatusOK, map[string]any{"module": module, "created": created, "existing": existing})
}

// fixtureNames returns the ordered DocType names of a fixture set.
func fixtureNames(fx []DocType) []string {
	names := make([]string, len(fx))
	for i, dt := range fx {
		names[i] = dt.Name
	}
	return names
}

// ---- Document handlers ----

func (s *svc) createDocument(c *zip.Ctx) error {
	acc, dt, err := s.access(c, rightCreate)
	if err != nil {
		return err
	}
	in, err := bindDoc(c)
	if err != nil {
		return err
	}
	// A Single has exactly one document; create and update are the SAME upsert,
	// guarded identically (draft-only). Route both through writeSingle.
	if dt.IsSingle {
		return s.writeSingle(c, acc, &dt, in, http.StatusCreated)
	}
	validated, err := s.store.validateDoc(c.Context(), acc.org, &dt, in, nil, "", false)
	if err != nil {
		return mapErr(err, "")
	}
	doc := Document{DocType: dt.Name, Data: validated}
	ev := s.event(acc.org, &dt, &doc, nil)
	if err := runHooks(c.Context(), ActionBeforeInsert, ev); err != nil {
		return hookErr(err)
	}
	if err := runHooks(c.Context(), ActionBeforeSave, ev); err != nil {
		return hookErr(err)
	}
	saved, err := s.store.CreateDocument(c.Context(), acc.org, &dt, doc.Data, stringField(in, "name"))
	if err != nil {
		return mapErr(err, "")
	}
	s.after(c.Context(), acc.org, &dt, &saved, nil)
	return c.JSON(http.StatusCreated, wireDoc(&dt, saved, nil))
}

func (s *svc) listDocuments(c *zip.Ctx) error {
	acc, dt, err := s.access(c, rightRead)
	if err != nil {
		return err
	}
	if dt.IsSingle {
		doc, err := s.getSingle(c.Context(), acc.org, &dt)
		if err != nil {
			return mapErr(err, "not found")
		}
		return c.JSON(http.StatusOK, map[string]any{"data": []any{wireDoc(&dt, doc, nil)}})
	}
	opts, fields, err := parseListOpts(c, &dt)
	if err != nil {
		return err
	}
	rows, err := s.store.ListDocuments(c.Context(), acc.org, dt.Name, opts)
	if err != nil {
		return zip.Errorf(http.StatusInternalServerError, "list: %v", err)
	}
	out := make([]any, 0, len(rows))
	for _, d := range rows {
		out = append(out, wireDoc(&dt, d, fields))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": out})
}

func (s *svc) getDocument(c *zip.Ctx) error {
	acc, dt, err := s.access(c, rightRead)
	if err != nil {
		return err
	}
	name := s.docName(c, &dt)
	if dt.IsSingle {
		doc, err := s.getSingle(c.Context(), acc.org, &dt)
		if err != nil {
			return mapErr(err, "document not found")
		}
		return c.JSON(http.StatusOK, wireDoc(&dt, doc, nil))
	}
	doc, err := s.store.GetDocument(c.Context(), acc.org, dt.Name, name)
	if err != nil {
		return mapErr(err, "document not found")
	}
	return c.JSON(http.StatusOK, wireDoc(&dt, doc, nil))
}

func (s *svc) updateDocument(c *zip.Ctx) error {
	acc, dt, err := s.access(c, rightWrite)
	if err != nil {
		return err
	}
	in, err := bindDoc(c)
	if err != nil {
		return err
	}
	if dt.IsSingle {
		return s.writeSingle(c, acc, &dt, in, http.StatusOK)
	}
	name := s.docName(c, &dt)

	prev, err := s.store.GetDocument(c.Context(), acc.org, dt.Name, name)
	if err != nil {
		return mapErr(err, "document not found")
	}
	if prev.DocStatus != 0 {
		return zip.Errorf(http.StatusConflict, "document is not a draft (docstatus %d); cannot edit", prev.DocStatus)
	}
	validated, err := s.store.validateDoc(c.Context(), acc.org, &dt, in, prev.Data, name, false)
	if err != nil {
		return mapErr(err, "")
	}
	doc := Document{DocType: dt.Name, Name: name, Data: validated, DocStatus: prev.DocStatus}
	ev := s.event(acc.org, &dt, &doc, &prev)
	if err := runHooks(c.Context(), ActionBeforeSave, ev); err != nil {
		return hookErr(err)
	}
	saved, err := s.store.UpdateDocument(c.Context(), acc.org, &dt, name, doc.Data)
	if err != nil {
		return mapErr(err, "document not found")
	}
	s.after(c.Context(), acc.org, &dt, &saved, &prev)
	return c.JSON(http.StatusOK, wireDoc(&dt, saved, nil))
}

func (s *svc) deleteDocument(c *zip.Ctx) error {
	acc, dt, err := s.access(c, rightDelete)
	if err != nil {
		return err
	}
	name := s.docName(c, &dt)
	prev, err := s.store.GetDocument(c.Context(), acc.org, dt.Name, name)
	if err != nil {
		return mapErr(err, "document not found")
	}
	if prev.DocStatus == 1 {
		return zip.Errorf(http.StatusConflict, "submitted document must be cancelled before deletion")
	}
	ev := s.event(acc.org, &dt, &prev, nil)
	if err := runHooks(c.Context(), ActionOnTrash, ev); err != nil {
		return hookErr(err)
	}
	deleted, err := s.store.DeleteDocument(c.Context(), acc.org, dt.Name, name)
	if err != nil {
		return zip.Errorf(http.StatusInternalServerError, "delete: %v", err)
	}
	if !deleted {
		return zip.ErrNotFound("document not found")
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *svc) submitDocument(c *zip.Ctx) error {
	return s.transition(c, rightSubmit, 0, 1, ActionOnSubmit)
}

func (s *svc) cancelDocument(c *zip.Ctx) error {
	return s.transition(c, rightCancel, 1, 2, ActionOnCancel)
}

// transition is the shared submit/cancel path: gate on the right, require the
// doctype be submittable, run the lifecycle hook (gate), then flip docstatus.
func (s *svc) transition(c *zip.Ctx, right string, from, to int, action string) error {
	acc, dt, err := s.access(c, right)
	if err != nil {
		return err
	}
	if !dt.IsSubmittable {
		return zip.ErrBadRequest("doctype is not submittable")
	}
	name := s.docName(c, &dt)
	doc, err := s.store.GetDocument(c.Context(), acc.org, dt.Name, name)
	if err != nil {
		return mapErr(err, "document not found")
	}
	if doc.DocStatus != from {
		return zip.Errorf(http.StatusConflict, "illegal docstatus transition from %d", doc.DocStatus)
	}
	ev := s.event(acc.org, &dt, &doc, nil)
	if err := runHooks(c.Context(), action, ev); err != nil {
		return hookErr(err)
	}
	saved, err := s.store.SetDocStatus(c.Context(), acc.org, dt.Name, name, from, to)
	if err != nil {
		return mapErr(err, "document not found")
	}
	return c.JSON(http.StatusOK, wireDoc(&dt, saved, nil))
}

func (s *svc) summary(c *zip.Ctx) error {
	acc, err := s.resolveAccess(c)
	if err != nil {
		return err
	}
	dts, err := s.store.ListDocTypes(c.Context(), acc.org)
	if err != nil {
		return zip.Errorf(http.StatusInternalServerError, "summary: %v", err)
	}
	var docs int
	for _, dt := range dts {
		n, err := s.store.CountDocuments(c.Context(), acc.org, dt.Name)
		if err != nil {
			return zip.Errorf(http.StatusInternalServerError, "summary: %v", err)
		}
		docs += n
	}
	return c.JSON(http.StatusOK, map[string]any{"doctypes": len(dts), "documents": docs})
}

// ---- shared handler plumbing ----

// access resolves the caller AND loads the target DocType, enforcing `right` in
// one place. It is the ONE gate every document handler passes through: 403 on no
// principal or denied permission, 404 on unknown doctype.
func (s *svc) access(c *zip.Ctx, right string) (access, DocType, error) {
	acc, err := s.resolveAccess(c)
	if err != nil {
		return access{}, DocType{}, err
	}
	dt, err := s.store.GetDocType(c.Context(), acc.org, pathParam(c, "doctype"))
	if err != nil {
		return access{}, DocType{}, mapErr(err, "doctype not found")
	}
	if !acc.can(&dt, right) {
		return access{}, DocType{}, zip.ErrForbidden("permission denied: " + right + " on " + dt.Name)
	}
	return acc, dt, nil
}

func (s *svc) event(org string, dt *DocType, doc, prev *Document) *Event {
	return &Event{Org: org, DocType: dt.Name, Doc: doc, Prev: prev, Meta: dt, Store: s.store, Logger: s.log}
}

// after runs after_save hooks; a failure is logged, not fatal (the write already
// committed — gates belong in before_save/on_submit, see hook.go).
func (s *svc) after(ctx context.Context, org string, dt *DocType, doc, prev *Document) {
	ev := s.event(org, dt, doc, prev)
	if err := runHooks(ctx, ActionAfterSave, ev); err != nil {
		s.log.Warn("after_save hook failed", "doctype", dt.Name, "name", doc.Name, "err", err)
	}
}

// docName returns the effective document name: the URL param, or the doctype name
// for a Single (a single always has exactly one document named after its type).
func (s *svc) docName(c *zip.Ctx, dt *DocType) string {
	if dt.IsSingle {
		return dt.Name
	}
	return pathParam(c, "name")
}

// pathParam reads a URL path parameter and percent-decodes it, so a segment that
// names a record containing reserved characters — a space ("Sales Invoice",
// "System Manager") arrives on the wire as %20 — is matched against its STORED
// value, not its raw encoding. The router (zip over fasthttp) runs with Fiber's
// default UnescapePath:false, so c.Param hands segments back verbatim; every
// framework path segment names a stored DocType/document/role, so decoding is
// correct HERE, in the ONE place the framework reads a path param. A malformed
// escape is left as-is — it simply won't match any stored name (an honest 404),
// never a panic. TrimSpace mirrors the naming rules (resolveName/Validate both
// trim), so " Foo " and "Foo" address the same record.
func pathParam(c *zip.Ctx, name string) string {
	raw := c.Param(name)
	if dec, err := url.PathUnescape(raw); err == nil {
		raw = dec
	}
	return strings.TrimSpace(raw)
}

// getSingle returns the Single's document, or a virtual empty draft when it has
// not been written yet (a single always "exists").
func (s *svc) getSingle(ctx context.Context, org string, dt *DocType) (Document, error) {
	doc, err := s.store.GetDocument(ctx, org, dt.Name, dt.Name)
	if err == errNotFound {
		return Document{Name: dt.Name, DocType: dt.Name, Data: map[string]any{}}, nil
	}
	return doc, err
}

// writeSingle upserts the ONE document of a Single DocType (name == doctype
// name), used by both POST and PUT. It enforces the SAME immutability as the
// non-Single path — a submitted/cancelled Single (docstatus != 0) is not editable
// (409) — and preserves a redacted Password across an unchanged update by passing
// the current data as `prev` to the validator. Without this guard UpsertSingle
// would silently mutate a submitted Single (the LOW-1 finding).
func (s *svc) writeSingle(c *zip.Ctx, acc access, dt *DocType, in map[string]any, okStatus int) error {
	cur, curErr := s.store.GetDocument(c.Context(), acc.org, dt.Name, dt.Name)
	var prev map[string]any
	if curErr == nil {
		if cur.DocStatus != 0 {
			return zip.Errorf(http.StatusConflict, "document is not a draft (docstatus %d); cannot edit", cur.DocStatus)
		}
		prev = cur.Data
	} else if curErr != errNotFound {
		return mapErr(curErr, "")
	}
	validated, err := s.store.validateDoc(c.Context(), acc.org, dt, in, prev, dt.Name, false)
	if err != nil {
		return mapErr(err, "")
	}
	doc := Document{DocType: dt.Name, Name: dt.Name, Data: validated}
	ev := s.event(acc.org, dt, &doc, nil)
	if err := runHooks(c.Context(), ActionBeforeSave, ev); err != nil {
		return hookErr(err)
	}
	saved, err := s.store.UpsertSingle(c.Context(), acc.org, dt, doc.Data)
	if err != nil {
		return mapErr(err, "")
	}
	s.after(c.Context(), acc.org, dt, &saved, nil)
	return c.JSON(okStatus, wireDoc(dt, saved, nil))
}

// ---- wire helpers ----

// wireDoc renders a Document for the HTTP response: the field data overlaid with
// the envelope keys (name/doctype/docstatus/createdAt/updatedAt). Password fields
// are REDACTED here — the ONE choke point every document response passes through,
// so a hash never leaves the process. `fields`, when non-nil, projects the data to
// the requested field set (envelope keys are always included).
func wireDoc(dt *DocType, d Document, fields []string) map[string]any {
	m := make(map[string]any, len(d.Data)+5)
	var keep map[string]bool
	if fields != nil {
		keep = make(map[string]bool, len(fields))
		for _, f := range fields {
			keep[f] = true
		}
	}
	for k, v := range d.Data {
		if keep != nil && !keep[k] {
			continue
		}
		m[k] = v
	}
	// Redact every Password field: set → marker, unset → omitted. Never the hash.
	for _, f := range dt.Fields {
		if f.Fieldtype != FieldPassword {
			continue
		}
		if v, ok := m[f.Fieldname]; ok {
			if s, _ := v.(string); s != "" {
				m[f.Fieldname] = redactedMarker
			} else {
				delete(m, f.Fieldname)
			}
		}
	}
	m["name"] = d.Name
	m["doctype"] = d.DocType
	m["docstatus"] = d.DocStatus
	m["createdAt"] = d.CreatedAt
	m["updatedAt"] = d.UpdatedAt
	return m
}

// bindDoc reads the request body into a field map with a size guard.
func bindDoc(c *zip.Ctx) (map[string]any, error) {
	b := c.Body()
	if len(b) > maxDocBytes {
		return nil, zip.ErrBadRequest("document body too large")
	}
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, zip.ErrBadRequest("invalid JSON body")
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// parseListOpts parses ?filters=&fields=&order_by=&limit= against a doctype's
// schema. Filter/sort field names must be declared (or name/docstatus); values are
// carried as bound parameters into the store. Unknown fields are a 400.
func parseListOpts(c *zip.Ctx, dt *DocType) (ListOpts, []string, error) {
	opts := ListOpts{Limit: defaultLimit}

	if raw := strings.TrimSpace(c.Query("filters")); raw != "" {
		var f map[string]any
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			return opts, nil, zip.ErrBadRequest("filters must be a JSON object")
		}
		opts.Filters = make(map[string]string, len(f))
		for k, v := range f {
			if !isQueryableField(dt, k) {
				return opts, nil, zip.ErrBadRequest("unknown filter field: " + k)
			}
			opts.Filters[k] = fmt.Sprint(v)
		}
	}

	var fields []string
	if raw := strings.TrimSpace(c.Query("fields")); raw != "" {
		for _, f := range splitFields(raw) {
			if _, ok := dt.field(f); !ok {
				return opts, nil, zip.ErrBadRequest("unknown field: " + f)
			}
			fields = append(fields, f)
		}
	}

	if raw := strings.TrimSpace(c.Query("order_by")); raw != "" {
		parts := strings.Fields(raw)
		field := parts[0]
		if !isQueryableField(dt, field) {
			return opts, nil, zip.ErrBadRequest("unknown order_by field: " + field)
		}
		opts.OrderField = field
		if len(parts) > 1 && strings.EqualFold(parts[1], "desc") {
			opts.Desc = true
		}
	} else {
		opts.Desc = true // default: most-recently-updated first
	}

	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			opts.Limit = n
		}
	}
	return opts, fields, nil
}

// isQueryableField reports whether a name may be filtered/sorted: a declared
// field, or the managed columns name/docstatus.
func isQueryableField(dt *DocType, name string) bool {
	if name == "name" || name == "docstatus" {
		return true
	}
	_, ok := dt.field(name)
	return ok
}

func splitFields(raw string) []string {
	// Accept a JSON array ["a","b"] or a comma list "a,b".
	if strings.HasPrefix(raw, "[") {
		var arr []string
		if err := json.Unmarshal([]byte(raw), &arr); err == nil {
			return arr
		}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// hookErr maps a hook abort to 422 (the gate rejected the operation).
func hookErr(err error) error {
	return zip.Errorf(http.StatusUnprocessableEntity, "%v", err)
}

// mapErr maps a store/validation error to the right HTTP status.
func mapErr(err error, notFoundMsg string) error {
	var ve *validationError
	switch {
	case errors.Is(err, errNotFound):
		if notFoundMsg == "" {
			notFoundMsg = "not found"
		}
		return zip.ErrNotFound(notFoundMsg)
	case errors.Is(err, errConflict):
		return zip.ErrConflict("already exists")
	case errors.Is(err, errBadRef):
		return zip.Errorf(http.StatusUnprocessableEntity, "referenced record not found in org")
	case errors.Is(err, errBadState):
		return zip.Errorf(http.StatusConflict, "illegal document state transition")
	case errors.As(err, &ve):
		return zip.ErrBadRequest(ve.msg)
	default:
		return zip.Errorf(http.StatusInternalServerError, "%v", err)
	}
}
