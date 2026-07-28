package framework

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	// github.com/hanzoai/sqlite is the ONE Hanzo SQLite driver: it registers
	// the "sqlite" database/sql name under both build tags (cgo →
	// mattn+SQLCipher, encrypted at rest; !cgo → pure-Go modernc). Importing
	// modernc directly instead would double-register "sqlite" under CGO and
	// panic at init. Blank import registers the driver.
	"github.com/hanzoai/doctype"
	_ "github.com/hanzoai/sqlite"
)

// Store is the framework database. ONE SQLite file ({DataDir}/framework.db) holds
// every org's DocTypes, documents, naming counters, and role assignments; tenant
// isolation is the `org` column, present on EVERY table and EVERY query. This is
// the ONE storage pattern (mirrors clients/crm, clients/cms). MaxOpenConns(1)
// serializes writes against the single-writer SQLite file.
type Store struct {
	db *sql.DB
	// watchState is the change log's in-process bookkeeping (see changes.go):
	// the wake channels of subscribers sharing this process, and when the log
	// was last trimmed. Its zero value is ready.
	watchState
}

// openStore opens (and migrates) the engine's database. `open` is the host's
// opener — nil means the plain hanzoai/sqlite driver, which is what a
// standalone app or a test wants; Hanzo Cloud injects an encrypted-at-rest one.
func openStore(path string, open OpenDBFunc) (*Store, error) {
	if open == nil {
		open = func(p string) (*sql.DB, error) { return sql.Open("sqlite", p) }
	}
	db, err := open(path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// migrate creates the framework tables. Idempotent (IF NOT EXISTS). Every table
// carries `org` — leading the primary key where it has one — so tenant isolation
// is a physical property of the schema, not merely a WHERE clause.
func (s *Store) migrate() error {
	const ddl = `
CREATE TABLE IF NOT EXISTS fw_doctypes (
  org            TEXT NOT NULL,
  name           TEXT NOT NULL,
  module         TEXT NOT NULL DEFAULT '',
  is_single      INTEGER NOT NULL DEFAULT 0,
  is_submittable INTEGER NOT NULL DEFAULT 0,
  autoname       TEXT NOT NULL DEFAULT '',
  title_field    TEXT NOT NULL DEFAULT '',
  fields         TEXT NOT NULL DEFAULT '[]',
  permissions    TEXT NOT NULL DEFAULT '[]',
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL,
  PRIMARY KEY (org, name)
);
CREATE INDEX IF NOT EXISTS ix_fw_doctypes_org_module ON fw_doctypes(org, module);

CREATE TABLE IF NOT EXISTS fw_documents (
  org        TEXT NOT NULL,
  doctype    TEXT NOT NULL,
  name       TEXT NOT NULL,
  docstatus  INTEGER NOT NULL DEFAULT 0,
  data       TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (org, doctype, name)
);
CREATE INDEX IF NOT EXISTS ix_fw_documents_org_dt_updated ON fw_documents(org, doctype, updated_at);

CREATE TABLE IF NOT EXISTS fw_series (
  org     TEXT NOT NULL,
  series  TEXT NOT NULL,
  current INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (org, series)
);

CREATE TABLE IF NOT EXISTS fw_roles (
  org  TEXT NOT NULL,
  usr  TEXT NOT NULL,
  role TEXT NOT NULL,
  PRIMARY KEY (org, usr, role)
);
CREATE INDEX IF NOT EXISTS ix_fw_roles_org ON fw_roles(org);

CREATE TABLE IF NOT EXISTS fw_locks (
  org        TEXT NOT NULL,
  lockkey    TEXT NOT NULL,
  holder     TEXT NOT NULL,
  expires_at INTEGER NOT NULL,   -- unix nanos; a lease auto-expires so a crashed holder never wedges
  PRIMARY KEY (org, lockkey)
);
`
	if _, err := s.db.Exec(ddl); err != nil {
		return fmt.Errorf("framework migrate: %w", err)
	}
	// The change log's schema lives with the change log (changes.go), so the one
	// file that owns the feed owns its table too.
	if _, err := s.db.Exec(changesDDL); err != nil {
		return fmt.Errorf("framework migrate changes: %w", err)
	}
	if _, err := s.db.Exec(presenceDDL); err != nil {
		return fmt.Errorf("framework migrate presence: %w", err)
	}
	return nil
}

// Close closes the underlying database. Idempotent-safe via sql.DB.
func (s *Store) Close() error { return s.db.Close() }

// ---- DocType registry ----

func (s *Store) CreateDocType(ctx context.Context, org string, dt DocType) (DocType, error) {
	dt.Normalize()
	now := time.Now().Unix()
	dt.CreatedAt, dt.UpdatedAt = now, now
	fieldsJSON, _ := json.Marshal(dt.Fields)
	permsJSON, _ := json.Marshal(dt.Perms)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO fw_doctypes (org,name,module,is_single,is_submittable,autoname,title_field,fields,permissions,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		org, dt.Name, dt.Module, b2i(dt.IsSingle), b2i(dt.IsSubmittable), dt.Autoname, dt.TitleField,
		string(fieldsJSON), string(permsJSON), now, now)
	if err != nil {
		if isUnique(err) {
			return DocType{}, ErrConflict
		}
		return DocType{}, fmt.Errorf("insert doctype: %w", err)
	}
	return dt, nil
}

func (s *Store) GetDocType(ctx context.Context, org, name string) (DocType, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name,module,is_single,is_submittable,autoname,title_field,fields,permissions,created_at,updated_at
		 FROM fw_doctypes WHERE org=? AND name=?`, org, name)
	dt, err := scanDocType(row)
	if errors.Is(err, sql.ErrNoRows) {
		// No stored (per-org) definition. An always-on module's fixture resolves
		// virtually so a fresh org uses the lane without a per-org install. Only the
		// DocType DEFINITION resolves this way — every document row stays physically
		// org-scoped (see always_on_isolation_test.go), so this exposes schema, never
		// another org's data.
		if fx, ok := doctype.AlwaysOn(name); ok {
			return fx, nil
		}
		return DocType{}, ErrNotFound
	}
	if err != nil {
		return DocType{}, fmt.Errorf("get doctype: %w", err)
	}
	return dt, nil
}

func (s *Store) ListDocTypes(ctx context.Context, org string) ([]DocType, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name,module,is_single,is_submittable,autoname,title_field,fields,permissions,created_at,updated_at
		 FROM fw_doctypes WHERE org=? ORDER BY name ASC`, org)
	if err != nil {
		return nil, fmt.Errorf("list doctypes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]DocType, 0, 16)
	seen := make(map[string]bool, 16)
	for rows.Next() {
		dt, err := scanDocType(rows)
		if err != nil {
			return nil, fmt.Errorf("scan doctype: %w", err)
		}
		out = append(out, dt)
		seen[dt.Name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Union the always-on fixtures this org has NOT customized (a stored row wins), so
	// the listing matches GetDocType. Then re-sort by name to preserve the ORDER BY
	// name ASC contract across the union.
	for _, fx := range doctype.AlwaysOnAll() {
		if !seen[fx.Name] {
			out = append(out, fx)
			seen[fx.Name] = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ReplaceDocType replaces an existing DocType's definition (PUT semantics). The
// documents already stored under it are left intact (a schema change never
// silently drops data); the next write validates against the new schema.
func (s *Store) ReplaceDocType(ctx context.Context, org string, dt DocType) (DocType, error) {
	dt.Normalize()
	fieldsJSON, _ := json.Marshal(dt.Fields)
	permsJSON, _ := json.Marshal(dt.Perms)
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE fw_doctypes SET module=?,is_single=?,is_submittable=?,autoname=?,title_field=?,fields=?,permissions=?,updated_at=?
		 WHERE org=? AND name=?`,
		dt.Module, b2i(dt.IsSingle), b2i(dt.IsSubmittable), dt.Autoname, dt.TitleField,
		string(fieldsJSON), string(permsJSON), now, org, dt.Name)
	if err != nil {
		return DocType{}, fmt.Errorf("update doctype: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return DocType{}, ErrNotFound
	}
	return s.GetDocType(ctx, org, dt.Name)
}

// DeleteDocType removes a DocType and ALL of its documents in one transaction —
// there are no orphaned documents without a schema. Returns false if absent.
func (s *Store) DeleteDocType(ctx context.Context, org, name string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM fw_doctypes WHERE org=? AND name=?`, org, name)
	if err != nil {
		return false, fmt.Errorf("delete doctype: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM fw_documents WHERE org=? AND doctype=?`, org, name); err != nil {
		return false, fmt.Errorf("delete documents: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return n > 0, nil
}

func scanDocType(sc interface{ Scan(...any) error }) (DocType, error) {
	var (
		dt                    DocType
		single, submittable   int
		fieldsJSON, permsJSON string
	)
	if err := sc.Scan(&dt.Name, &dt.Module, &single, &submittable, &dt.Autoname, &dt.TitleField,
		&fieldsJSON, &permsJSON, &dt.CreatedAt, &dt.UpdatedAt); err != nil {
		return DocType{}, err
	}
	dt.IsSingle = single != 0
	dt.IsSubmittable = submittable != 0
	_ = json.Unmarshal([]byte(fieldsJSON), &dt.Fields)
	_ = json.Unmarshal([]byte(permsJSON), &dt.Perms)
	if dt.Fields == nil {
		dt.Fields = []DocField{}
	}
	if dt.Perms == nil {
		dt.Perms = []DocPerm{}
	}
	return dt, nil
}

// ---- Documents ----

// Document is a schemaless record: Data holds the validated field values, keyed
// by fieldname. The HTTP layer serializes it via wireDoc (Data + envelope keys),
// so this internal shape is the ONE representation the store works with.
type Document struct {
	Name      string
	DocType   string
	DocStatus int
	Data      map[string]any
	CreatedAt int64
	UpdatedAt int64
}

// CreateDocument names and inserts a validated document at docstatus 0. `data`
// is the canonical, already-validated field map (the service calls validateDoc
// first). Naming follows dt.Autoname; series naming increments a per-org counter
// inside the same transaction so concurrent creates never collide.
func (s *Store) CreateDocument(ctx context.Context, org string, dt *DocType, data map[string]any, requestedName string) (Document, error) {
	now := time.Now().Unix()
	blob, err := json.Marshal(data)
	if err != nil {
		return Document{}, fmt.Errorf("marshal data: %w", err)
	}

	insert := func(ctx context.Context, x execer, name string) error {
		_, err := x.ExecContext(ctx,
			`INSERT INTO fw_documents (org,doctype,name,docstatus,data,created_at,updated_at) VALUES (?,?,?,0,?,?,?)`,
			org, dt.Name, name, string(blob), now, now)
		return err
	}

	// Both naming paths run in a transaction so the change-log row (changes.go)
	// is atomic with the insert it describes: a subscriber can never be told
	// about a document that failed to land, nor miss one that did.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Document{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var name string
	if doctype.IsSeries(dt.Autoname) {
		ser := doctype.ExpandSeries(dt.Autoname, time.Now())
		n, err := nextSeries(ctx, tx, org, ser.Key)
		if err != nil {
			return Document{}, err
		}
		name = ser.Format(n)
	} else if name, err = doctype.ResolveName(dt, data, requestedName); err != nil {
		return Document{}, err
	}
	if err := insert(ctx, tx, name); err != nil {
		if isUnique(err) {
			return Document{}, ErrConflict
		}
		return Document{}, fmt.Errorf("insert document: %w", err)
	}
	if err := s.appendChange(ctx, tx, org, dt.Name, name, ChangeCreated, 0); err != nil {
		return Document{}, err
	}
	if err := tx.Commit(); err != nil {
		return Document{}, fmt.Errorf("commit: %w", err)
	}
	s.committed(ctx)
	return Document{Name: name, DocType: dt.Name, Data: data, CreatedAt: now, UpdatedAt: now}, nil
}

// UpsertSingle writes THE single document of a Single DocType (name == doctype
// name), creating or replacing it. A Single never has more than one row per org.
func (s *Store) UpsertSingle(ctx context.Context, org string, dt *DocType, data map[string]any) (Document, error) {
	now := time.Now().Unix()
	blob, err := json.Marshal(data)
	if err != nil {
		return Document{}, fmt.Errorf("marshal data: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Document{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO fw_documents (org,doctype,name,docstatus,data,created_at,updated_at) VALUES (?,?,?,0,?,?,?)
		 ON CONFLICT(org,doctype,name) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at`,
		org, dt.Name, dt.Name, string(blob), now, now); err != nil {
		return Document{}, fmt.Errorf("upsert single: %w", err)
	}
	// A Single's create and update are the same write, so the feed reports the
	// same action for both: a subscriber re-reads the one document either way.
	if err := s.appendChange(ctx, tx, org, dt.Name, dt.Name, ChangeUpdated, 0); err != nil {
		return Document{}, err
	}
	if err := tx.Commit(); err != nil {
		return Document{}, fmt.Errorf("commit: %w", err)
	}
	s.committed(ctx)
	return s.GetDocument(ctx, org, dt.Name, dt.Name)
}

func (s *Store) GetDocument(ctx context.Context, org, dtName, name string) (Document, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name,docstatus,data,created_at,updated_at FROM fw_documents WHERE org=? AND doctype=? AND name=?`,
		org, dtName, name)
	d, err := scanDocument(row, dtName)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("get document: %w", err)
	}
	return d, nil
}

// ListOpts is a parsed, validated list query. Filters keys are declared field
// names (or the special "name"/"docstatus"); OrderField is a resolved sort key.
type ListOpts struct {
	Filters    map[string]string
	OrderField string // "" → updated_at
	Desc       bool
	Limit      int
}

// ListDocuments returns the org's documents of a doctype, applying equality
// filters (via json_extract, all values BOUND), an order key, and a bounded
// limit. Every value is a bound parameter and every field name is validated
// against the doctype's schema before it reaches a json path.
func (s *Store) ListDocuments(ctx context.Context, org, dtName string, opts ListOpts) ([]Document, error) {
	var (
		where = []string{"org=?", "doctype=?"}
		args  = []any{org, dtName}
	)
	for field, val := range opts.Filters {
		switch field {
		case "name":
			where = append(where, "name=?")
			args = append(args, val)
		case "docstatus":
			where = append(where, "docstatus=?")
			args = append(args, val)
		default:
			// json_extract's path arg is a BOUND concatenation — no interpolation.
			where = append(where, "CAST(json_extract(data,'$.'||?) AS TEXT)=?")
			args = append(args, field, val)
		}
	}

	order := "updated_at"
	switch opts.OrderField {
	case "", "modified", "updated_at":
		order = "updated_at"
	case "creation", "created_at":
		order = "created_at"
	case "name":
		order = "name"
	case "docstatus":
		order = "docstatus"
	default:
		order = "json_extract(data,'$.'||?)"
	}
	dir := "DESC"
	if !opts.Desc {
		dir = "ASC"
	}
	limit := opts.Limit
	if limit <= 0 || limit > doctype.MaxLimit {
		limit = doctype.DefaultLimit
	}

	q := `SELECT name,docstatus,data,created_at,updated_at FROM fw_documents WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY ` + order + ` ` + dir + ` LIMIT ?`
	if strings.Contains(order, "json_extract") {
		args = append(args, opts.OrderField)
	}
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]Document, 0, 16)
	for rows.Next() {
		d, err := scanDocument(rows, dtName)
		if err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateDocument replaces a document's data. Only a DRAFT (docstatus 0) is
// editable — a submitted/cancelled document is immutable (ErrBadState) so the
// submit lifecycle can't be bypassed by a plain PUT. `data` is already validated.
func (s *Store) UpdateDocument(ctx context.Context, org string, dt *DocType, name string, data map[string]any) (Document, error) {
	blob, err := json.Marshal(data)
	if err != nil {
		return Document{}, fmt.Errorf("marshal data: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Document{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var status int
	err = tx.QueryRowContext(ctx, `SELECT docstatus FROM fw_documents WHERE org=? AND doctype=? AND name=?`,
		org, dt.Name, name).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("lock document: %w", err)
	}
	if status != 0 {
		return Document{}, ErrBadState
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `UPDATE fw_documents SET data=?, updated_at=? WHERE org=? AND doctype=? AND name=?`,
		string(blob), now, org, dt.Name, name); err != nil {
		return Document{}, fmt.Errorf("update document: %w", err)
	}
	if err := s.appendChange(ctx, tx, org, dt.Name, name, ChangeUpdated, status); err != nil {
		return Document{}, err
	}
	if err := tx.Commit(); err != nil {
		return Document{}, fmt.Errorf("commit: %w", err)
	}
	s.committed(ctx)
	return s.GetDocument(ctx, org, dt.Name, name)
}

// SetDocStatus transitions a document from `from` to `to` atomically, verifying
// the current status equals `from` (else ErrBadState). This is the ONE path for
// submit (0→1) and cancel (1→2); the check-and-set is inside a transaction so two
// concurrent submits can't both win.
func (s *Store) SetDocStatus(ctx context.Context, org, dtName, name string, from, to int) (Document, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Document{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var status int
	err = tx.QueryRowContext(ctx, `SELECT docstatus FROM fw_documents WHERE org=? AND doctype=? AND name=?`,
		org, dtName, name).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("lock document: %w", err)
	}
	if status != from {
		return Document{}, ErrBadState
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `UPDATE fw_documents SET docstatus=?, updated_at=? WHERE org=? AND doctype=? AND name=?`,
		to, now, org, dtName, name); err != nil {
		return Document{}, fmt.Errorf("set docstatus: %w", err)
	}
	if err := s.appendChange(ctx, tx, org, dtName, name, docStatusAction(to), to); err != nil {
		return Document{}, err
	}
	if err := tx.Commit(); err != nil {
		return Document{}, fmt.Errorf("commit: %w", err)
	}
	s.committed(ctx)
	return s.GetDocument(ctx, org, dtName, name)
}

// DeleteDocument removes a document by key. Returns false if absent. The service
// enforces the lifecycle guard (a submitted doc must be cancelled first).
func (s *Store) DeleteDocument(ctx context.Context, org, dtName, name string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM fw_documents WHERE org=? AND doctype=? AND name=?`, org, dtName, name)
	if err != nil {
		return false, fmt.Errorf("delete document: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Nothing was removed, so nothing changed: no change row, no wake.
		return false, nil
	}
	if err := s.appendChange(ctx, tx, org, dtName, name, ChangeDeleted, 0); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	s.committed(ctx)
	return true, nil
}

// CountDocuments returns the org's document count for a doctype (real, per-org).
func (s *Store) CountDocuments(ctx context.Context, org, dtName string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fw_documents WHERE org=? AND doctype=?`, org, dtName).Scan(&n)
	return n, err
}

func scanDocument(sc interface{ Scan(...any) error }, dtName string) (Document, error) {
	var (
		d    Document
		blob string
	)
	if err := sc.Scan(&d.Name, &d.DocStatus, &blob, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return Document{}, err
	}
	d.DocType = dtName
	if err := json.Unmarshal([]byte(blob), &d.Data); err != nil {
		return Document{}, fmt.Errorf("unmarshal data: %w", err)
	}
	if d.Data == nil {
		d.Data = map[string]any{}
	}
	return d, nil
}

// documentExists reports whether (org, doctype, name) exists — the in-org Link
// reference check. table/column are constants; name is a bound parameter.
func (s *Store) documentExists(ctx context.Context, org, dtName, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM fw_documents WHERE org=? AND doctype=? AND name=?`, org, dtName, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("document exists: %w", err)
	}
	return true, nil
}

// fieldValueTaken reports whether another document in (org, doctype) already
// carries `value` for `field` — the Unique-field check. `field` is validated
// against the schema before this call and BOUND into the json path; excludeName
// lets an update skip the row being updated.
func (s *Store) fieldValueTaken(ctx context.Context, org, dtName, field, value, excludeName string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM fw_documents WHERE org=? AND doctype=? AND name<>? AND CAST(json_extract(data,'$.'||?) AS TEXT)=? LIMIT 1`,
		org, dtName, excludeName, field, value).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("unique check: %w", err)
	}
	return true, nil
}

// ---- Naming series counter ----

// nextSeries atomically increments and returns the per-org counter for a series
// key. Runs inside the caller's create transaction so the counter and the insert
// commit together (no gaps on rollback, no collisions across concurrent creates).
func nextSeries(ctx context.Context, tx *sql.Tx, org, key string) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO fw_series (org,series,current) VALUES (?,?,1)
		 ON CONFLICT(org,series) DO UPDATE SET current = current + 1`, org, key); err != nil {
		return 0, fmt.Errorf("bump series: %w", err)
	}
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT current FROM fw_series WHERE org=? AND series=?`, org, key).Scan(&n); err != nil {
		return 0, fmt.Errorf("read series: %w", err)
	}
	return n, nil
}

// ---- Roles (per-org assignments) ----

// Role is a (user, role) assignment within an org.
type Role struct {
	User string `json:"user"`
	Role string `json:"role"`
}

func (s *Store) ListRoles(ctx context.Context, org string) ([]Role, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT usr, role FROM fw_roles WHERE org=? ORDER BY usr, role`, org)
	if err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]Role, 0, 16)
	for rows.Next() {
		var r Role
		if err := rows.Scan(&r.User, &r.Role); err != nil {
			return nil, fmt.Errorf("scan role: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) AssignRole(ctx context.Context, org, user, role string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO fw_roles (org,usr,role) VALUES (?,?,?) ON CONFLICT(org,usr,role) DO NOTHING`, org, user, role)
	if err != nil {
		return fmt.Errorf("assign role: %w", err)
	}
	return nil
}

// SeedOwnerIfUnowned atomically grants `user` the System Manager role IFF the org
// has NO role assignment yet — the trust-on-first-use owner seed. It is a SINGLE
// conditional INSERT (INSERT ... SELECT ... WHERE NOT EXISTS), so the "is the org
// unowned?" test and the insert are one statement: under concurrency EXACTLY ONE
// caller's row lands (the rest match an org that now has a role and insert
// nothing). Returns whether THIS caller became the seeded owner (RowsAffected==1).
//
// This replaces a check-then-insert (a SELECT for existing roles, then an INSERT)
// whose window let several simultaneous first-callers each seed themselves System
// Manager (Red measured 3–6). A UNIQUE index is deliberately NOT used: multiple SMs
// are legitimate later, granted explicitly via AssignRole — only the AUTOMATIC
// first-seed must be singular.
func (s *Store) SeedOwnerIfUnowned(ctx context.Context, org, user string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO fw_roles (org, usr, role)
		 SELECT ?, ?, ?
		 WHERE NOT EXISTS (SELECT 1 FROM fw_roles WHERE org = ?)`,
		org, user, doctype.RoleSystemManager, org)
	if err != nil {
		return false, fmt.Errorf("seed owner: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) RevokeRole(ctx context.Context, org, user, role string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM fw_roles WHERE org=? AND usr=? AND role=?`, org, user, role)
	if err != nil {
		return false, fmt.Errorf("revoke role: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RolesFor returns the roles assigned to a user in an org.
func (s *Store) RolesFor(ctx context.Context, org, user string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT role FROM fw_roles WHERE org=? AND usr=?`, org, user)
	if err != nil {
		return nil, fmt.Errorf("roles for: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("scan role: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- small helpers ----

// execer is the common subset of *sql.DB and *sql.Tx used by insert().
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isUnique reports whether err is a SQLite UNIQUE/primary-key violation.
func isUnique(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "constraint failed")
}
