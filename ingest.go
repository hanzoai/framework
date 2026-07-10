package framework

import (
	"context"
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
	if s == nil || s.store == nil {
		return Ingested{}, fmt.Errorf("framework: not mounted")
	}
	if org == "" {
		return Ingested{}, fmt.Errorf("framework.Ingest: empty org")
	}

	dt, err := s.store.GetDocType(ctx, org, doctype)
	if err != nil {
		return Ingested{}, fmt.Errorf("framework.Ingest: doctype %q: %w", doctype, err)
	}

	// Single doctypes are upserts, not appends; a connector ingests many documents
	// into a normal (non-single) doctype (kb-source), so reject Single here rather
	// than silently overwriting the one row.
	if dt.IsSingle {
		return Ingested{}, fmt.Errorf("framework.Ingest: %q is a Single doctype", doctype)
	}

	validated, err := s.store.validateDoc(ctx, org, &dt, data, nil, "", false)
	if err != nil {
		return Ingested{}, err
	}
	doc := Document{DocType: dt.Name, Data: validated}
	ev := s.event(org, &dt, &doc, nil)
	if err := runHooks(ctx, ActionBeforeInsert, ev); err != nil {
		return Ingested{}, err
	}
	if err := runHooks(ctx, ActionBeforeSave, ev); err != nil {
		return Ingested{}, err
	}
	saved, err := s.store.CreateDocument(ctx, org, &dt, doc.Data, requestedName)
	if err != nil {
		return Ingested{}, err
	}
	// after_save runs the indexing hook (non-fatal, logged) exactly as on the HTTP path.
	s.after(ctx, org, &dt, &saved, nil)
	return Ingested{Org: org, DocType: dt.Name, Name: saved.Name}, nil
}

// Installed reports whether `doctype` exists in `org` (i.e. the module was
// installed). A connector checks this before a sync so it can return an honest
// "install the kb module first" rather than a doctype-not-found error mid-sync.
func Installed(ctx context.Context, org, doctype string) bool {
	s := mounted
	if s == nil || s.store == nil {
		return false
	}
	_, err := s.store.GetDocType(ctx, org, doctype)
	return err == nil
}

// UpdateData replaces the data of an existing document (draft only), running the
// before_save + after_save hooks — the in-process twin of the HTTP PUT. A connector
// uses it to refresh an already-ingested kb-source (same external_id) on re-sync so
// the vector point is updated in place rather than duplicated. `name` is the
// engine-assigned document name from a prior Ingest.
func UpdateData(ctx context.Context, org, doctype, name string, data map[string]any) error {
	s := mounted
	if s == nil || s.store == nil {
		return fmt.Errorf("framework: not mounted")
	}
	dt, err := s.store.GetDocType(ctx, org, doctype)
	if err != nil {
		return fmt.Errorf("framework.UpdateData: doctype %q: %w", doctype, err)
	}
	prev, err := s.store.GetDocument(ctx, org, doctype, name)
	if err != nil {
		return err
	}
	validated, err := s.store.validateDoc(ctx, org, &dt, data, prev.Data, name, false)
	if err != nil {
		return err
	}
	doc := Document{Name: name, DocType: dt.Name, Data: validated}
	ev := s.event(org, &dt, &doc, &prev)
	if err := runHooks(ctx, ActionBeforeSave, ev); err != nil {
		return err
	}
	saved, err := s.store.UpdateDocument(ctx, org, &dt, name, doc.Data)
	if err != nil {
		return err
	}
	s.after(ctx, org, &dt, &saved, &prev)
	return nil
}

// FindByField returns the name of the first document in (org, doctype) whose
// `field` equals `value`, or "" if none. A connector uses it to find an existing
// kb-source by external_id (idempotent re-sync: update in place vs. create new).
// `field` is validated against the doctype schema by ListDocuments' bound json path.
func FindByField(ctx context.Context, org, doctype, field, value string) (string, error) {
	s := mounted
	if s == nil || s.store == nil {
		return "", fmt.Errorf("framework: not mounted")
	}
	docs, err := s.store.ListDocuments(ctx, org, doctype, ListOpts{
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

// Search is the in-process, org-scoped document list a first-party subsystem (the
// KB retrieval surface) uses to hydrate search hits or count ingested docs without
// re-implementing the store query. It is a thin, validated pass-through to
// ListDocuments — every result is physically scoped to `org`.
func Search(ctx context.Context, org, doctype string, filters map[string]string, limit int) ([]Document, error) {
	s := mounted
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("framework: not mounted")
	}
	return s.store.ListDocuments(ctx, org, doctype, ListOpts{Filters: filters, Limit: limit})
}
