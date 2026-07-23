package framework

import (
	"context"
	"errors"
	"fmt"
)

// ingest.go is the ONE in-process document-create path for first-party producers
// that are INSIDE the trust boundary but not on an HTTP request — today the KB app
// lane's connector sync (Slack/GitHub/Google), which pulls external documents and
// files them as framework documents so the SAME after_save hook indexes them.
//
// It reuses the EXACT create pipeline the HTTP handler runs — validate →
// before_insert → before_save → insert → after_save — so there is no second write
// path and no way for a document created off-request to skip validation or the
// lifecycle hooks. The ONLY difference from the HTTP path is the permission gate:
// there is no request principal, so the caller supplies the already-VALIDATED org
// (its own tenant, resolved once upstream via principal.Org) and vouches, as
// first-party Go, that the write belongs to that tenant. Every store operation is
// still physically scoped by that org, so a connector can only ever write into the
// org it is syncing.

// Ingested is the minimal result of an in-process create: the org, doctype, and
// the engine-assigned document name (a connector records this to key incremental
// re-syncs).
type Ingested struct {
	Org     string
	DocType string
	Name    string
}

// Ingest creates a document of `doctype` in `org` from already-trusted field data,
// running the full validate + lifecycle-hook pipeline (so the after_save indexing
// hook fires exactly as it does for an HTTP create). `org` MUST be a validated
// tenant the caller already resolved — Ingest does not derive it. `requestedName`
// is used only for prompt/single naming (empty for the common autoname/hash case).
//
// It errors ("framework: not mounted") if called before the subsystem is mounted,
// and surfaces validation/lifecycle/store errors verbatim so a connector can record
// them on its connection status.
func Ingest(ctx context.Context, org, doctype string, data map[string]any, requestedName string) (Ingested, error) {
	s := mounted
	if s == nil || s.State.store == nil {
		return Ingested{}, fmt.Errorf("framework: not mounted")
	}
	if org == "" {
		return Ingested{}, fmt.Errorf("framework.Ingest: empty org")
	}

	dt, err := s.State.store.GetDocType(ctx, org, doctype)
	if err != nil {
		return Ingested{}, fmt.Errorf("framework.Ingest: doctype %q: %w", doctype, err)
	}

	// Single doctypes are upserts, not appends; a connector ingests many documents
	// into a normal (non-single) doctype (kb-source), so reject Single here rather
	// than silently overwriting the one row.
	if dt.IsSingle {
		return Ingested{}, fmt.Errorf("framework.Ingest: %q is a Single doctype", doctype)
	}

	validated, err := s.State.store.validateDoc(ctx, org, &dt, data, nil, "", false)
	if err != nil {
		return Ingested{}, err
	}
	doc := Document{DocType: dt.Name, Data: validated}
	ev := event(s, org, &dt, &doc, nil)
	if err := runHooks(ctx, ActionBeforeInsert, ev); err != nil {
		return Ingested{}, err
	}
	if err := runHooks(ctx, ActionBeforeSave, ev); err != nil {
		return Ingested{}, err
	}
	saved, err := s.State.store.CreateDocument(ctx, org, &dt, doc.Data, requestedName)
	if err != nil {
		return Ingested{}, err
	}
	// after_save runs the indexing hook (non-fatal, logged) exactly as on the HTTP path.
	after(s, ctx, org, &dt, &saved, nil)
	return Ingested{Org: org, DocType: dt.Name, Name: saved.Name}, nil
}

// Installed reports whether `doctype` exists in `org` (i.e. the module was
// installed). A connector checks this before a sync so it can return an honest
// "install the kb module first" rather than a doctype-not-found error mid-sync.
func Installed(ctx context.Context, org, doctype string) bool {
	s := mounted
	if s == nil || s.State.store == nil {
		return false
	}
	_, err := s.State.store.GetDocType(ctx, org, doctype)
	return err == nil
}

// ModuleInstalled reports whether the content model of `module` resolves for `org`
// — the org installed the lane (or the lane is always-on). It is the
// module-granularity sibling of Installed: an observe/growth reader asks "does this
// org run cms/erp?" without importing the framework store. A module is present when
// ANY of its registered DocType fixtures resolves for the org (GetDocType satisfies
// an always-on fixture for every org and an opt-in fixture only where the org ran
// the install). Org-scoped (each GetDocType keys on `org`) and nil-safe: an
// unmounted framework, an unknown module, or a store miss all yield false — never a
// spurious true and never any org's data (only the boolean).
func ModuleInstalled(ctx context.Context, org, module string) bool {
	s := mounted
	if s == nil || s.State.store == nil {
		return false
	}
	for _, dt := range moduleFixtures(module) {
		if _, err := s.State.store.GetDocType(ctx, org, dt.Name); err == nil {
			return true
		}
	}
	return false
}

// UpdateData replaces the data of an existing document (draft only), running the
// before_save + after_save hooks — the in-process twin of the HTTP PUT. A connector
// uses it to refresh an already-ingested kb-source (same external_id) on re-sync so
// the vector point is updated in place rather than duplicated. `name` is the
// engine-assigned document name from a prior Ingest.
func UpdateData(ctx context.Context, org, doctype, name string, data map[string]any) error {
	s := mounted
	if s == nil || s.State.store == nil {
		return fmt.Errorf("framework: not mounted")
	}
	dt, err := s.State.store.GetDocType(ctx, org, doctype)
	if err != nil {
		return fmt.Errorf("framework.UpdateData: doctype %q: %w", doctype, err)
	}
	prev, err := s.State.store.GetDocument(ctx, org, doctype, name)
	if err != nil {
		return err
	}
	validated, err := s.State.store.validateDoc(ctx, org, &dt, data, prev.Data, name, false)
	if err != nil {
		return err
	}
	doc := Document{Name: name, DocType: dt.Name, Data: validated}
	ev := event(s, org, &dt, &doc, &prev)
	if err := runHooks(ctx, ActionBeforeSave, ev); err != nil {
		return err
	}
	saved, err := s.State.store.UpdateDocument(ctx, org, &dt, name, doc.Data)
	if err != nil {
		return err
	}
	after(s, ctx, org, &dt, &saved, &prev)
	return nil
}

// FindByField returns the name of the first document in (org, doctype) whose
// `field` equals `value`, or "" if none. A connector uses it to find an existing
// kb-source by external_id (idempotent re-sync: update in place vs. create new).
// `field` is validated against the doctype schema by ListDocuments' bound json path.
func FindByField(ctx context.Context, org, doctype, field, value string) (string, error) {
	s := mounted
	if s == nil || s.State.store == nil {
		return "", fmt.Errorf("framework: not mounted")
	}
	docs, err := s.State.store.ListDocuments(ctx, org, doctype, ListOpts{
		Filters: map[string]string{field: value},
		Limit:   1,
	})
	if err != nil {
		return "", err
	}
	if len(docs) == 0 {
		return "", nil
	}
	return docs[0].Name, nil
}

// Delete removes a document in-process — the twin of the HTTP DELETE: it loads the
// document, runs the on_trash gate hooks (a returned error aborts the delete), then
// removes the row. `org` MUST be a validated tenant the caller already resolved. A
// first-party subsystem (the KB lane reconciling wikilink edges when a page is
// re-saved or trashed) uses it so edge cleanup runs the SAME lifecycle path as any
// other delete — never a forked write path. It returns ErrNotFound when the
// document does not exist (idempotent from the caller's view: a missing edge is a
// no-op).
func Delete(ctx context.Context, org, doctype, name string) error {
	s := mounted
	if s == nil || s.State.store == nil {
		return fmt.Errorf("framework: not mounted")
	}
	if org == "" {
		return fmt.Errorf("framework.Delete: empty org")
	}
	dt, err := s.State.store.GetDocType(ctx, org, doctype)
	if err != nil {
		return fmt.Errorf("framework.Delete: doctype %q: %w", doctype, err)
	}
	prev, err := s.State.store.GetDocument(ctx, org, doctype, name)
	if err != nil {
		return err
	}
	ev := event(s, org, &dt, &prev, nil)
	if err := runHooks(ctx, ActionOnTrash, ev); err != nil {
		return err
	}
	deleted, err := s.State.store.DeleteDocument(ctx, org, doctype, name)
	if err != nil {
		return err
	}
	if !deleted {
		return errNotFound
	}
	return nil
}

// Search is the in-process, org-scoped document list a first-party subsystem (the
// KB retrieval surface) uses to hydrate search hits or count ingested docs without
// re-implementing the store query. It is a thin, validated pass-through to
// ListDocuments — every result is physically scoped to `org`.
func Search(ctx context.Context, org, doctype string, filters map[string]string, limit int) ([]Document, error) {
	s := mounted
	if s == nil || s.State.store == nil {
		return nil, fmt.Errorf("framework: not mounted")
	}
	return s.State.store.ListDocuments(ctx, org, doctype, ListOpts{Filters: filters, Limit: limit})
}

// Get returns a single document by name in (org, doctype) — the read-one twin of
// Search for a first-party in-process reader (the content lane reads the current
// document to compute a lifecycle transition). `org` MUST be a validated tenant the
// caller already resolved. It returns ErrNotFound when the document does not exist, so
// the caller can answer 404 rather than 500.
func Get(ctx context.Context, org, doctype, name string) (Document, error) {
	s := mounted
	if s == nil || s.State.store == nil {
		return Document{}, fmt.Errorf("framework: not mounted")
	}
	return s.State.store.GetDocument(ctx, org, doctype, name)
}

// Exported error sentinels let an in-process caller (e.g. the content lane) classify
// the errors Ingest/Get/UpdateData/Search return WITHOUT string-matching. They alias
// the package's internal sentinels — the ONE definition stays in store.go, these are
// just the public handles.
var (
	ErrNotFound = errNotFound
	ErrConflict = errConflict
	ErrBadRef   = errBadRef
)

// IsValidationError reports whether err is a document-schema violation (the 400-class
// error validateDoc returns), so an in-process caller maps it to Bad Request rather
// than a 500 — the same *validationError the HTTP layer maps via mapErr.
func IsValidationError(err error) bool {
	var ve *validationError
	return errors.As(err, &ve)
}
