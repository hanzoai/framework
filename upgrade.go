package framework

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hanzoai/doctype"
)

// upgrade.go moves a database from the name-keyed schema to the address-keyed
// one, once.
//
// Before, a DocType was keyed by name alone and a lane kept its module OUT of
// collisions by writing the module into the name as well: "kb-page" in module
// "kb", "hd-ticket" in module "help". The module was therefore stored twice, and
// the two copies could disagree. Now the key IS the pair, the name is the bare
// kind, and the address module.name is the single rendering of it.
//
// The rename is derived from the REGISTRY, never from a table of old spellings:
// a stored name whose leading dash-token can be dropped to reach a fixture the
// row's own module declares becomes that fixture. "kb-page" in module kb reaches
// "page"; "erp-stock-item" reaches "stock-item" and not "item", because the strip
// is at the FIRST dash, not the longest suffix. A name that reaches nothing —
// "Page" in cms, or an org's own "Test Space" — is left exactly as it is, which
// is why a lane that never prefixed anything needs no exception here.
//
// It runs before the DDL and is a no-op twice over: on a database that has no
// old table, and on one already carrying the module column.

// upgrade rebuilds fw_doctypes and fw_documents on the address key. SQLite
// cannot ALTER a primary key, so this is the create-copy-drop-rename recipe, in
// one transaction: either the whole estate moves or none of it does.
func (s *Store) upgrade() error {
	ctx := context.Background()
	old, err := s.stale(ctx)
	if err != nil || !old {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("upgrade: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(doctypesDDL, "fw_doctypes_next")+fmt.Sprintf(documentsDDL, "fw_documents_next")); err != nil {
		return fmt.Errorf("upgrade: staging tables: %w", err)
	}
	moved, err := s.upgradeDocTypes(ctx, tx)
	if err != nil {
		return err
	}
	if err := s.upgradeDocuments(ctx, tx, moved); err != nil {
		return err
	}
	for _, stmt := range []string{
		`DROP TABLE fw_doctypes`,
		`DROP TABLE fw_documents`,
		`ALTER TABLE fw_doctypes_next RENAME TO fw_doctypes`,
		`ALTER TABLE fw_documents_next RENAME TO fw_documents`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("upgrade: %s: %w", stmt, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("upgrade: commit: %w", err)
	}
	return nil
}

// stale reports that the database holds the old schema: fw_documents exists and
// has no module column. Reading the shape is the whole version signal — it says
// what is actually there, which a version counter only claims.
func (s *Store) stale(ctx context.Context) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info('fw_documents')`)
	if err != nil {
		return false, fmt.Errorf("upgrade: read schema: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var cols int
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return false, err
		}
		cols++
		if col == "module" {
			return false, nil
		}
	}
	return cols > 0, rows.Err()
}

// upgradeDocTypes copies every DocType onto the address key, renaming it and
// rewriting its Link/Table targets. It returns what each (org, old name) became,
// which is the only thing that can tell the documents where they went.
func (s *Store) upgradeDocTypes(ctx context.Context, tx *sql.Tx) (map[[2]string]doctype.ID, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT org,name,module,is_single,is_submittable,autoname,title_field,fields,permissions,created_at,updated_at FROM fw_doctypes`)
	if err != nil {
		return nil, fmt.Errorf("upgrade: read doctypes: %w", err)
	}
	type row struct {
		org                   string
		id                    doctype.ID
		single, submittable   int
		autoname, titleField  string
		fieldsJSON, permsJSON string
		created, updated      int64
	}
	var (
		kept  []row
		moved = map[[2]string]doctype.ID{}
	)
	for rows.Next() {
		var (
			r   row
			old string
		)
		if err := rows.Scan(&r.org, &old, &r.id.Module, &r.single, &r.submittable, &r.autoname,
			&r.titleField, &r.fieldsJSON, &r.permsJSON, &r.created, &r.updated); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("upgrade: scan doctype: %w", err)
		}
		r.id.Name = kind(r.id.Module, old)
		moved[[2]string{r.org, old}] = r.id
		r.fieldsJSON = retarget(r.id.Module, r.fieldsJSON)
		kept = append(kept, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	for _, r := range kept {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO fw_doctypes_next (org,name,module,is_single,is_submittable,autoname,title_field,fields,permissions,created_at,updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			r.org, r.id.Name, r.id.Module, r.single, r.submittable, r.autoname, r.titleField,
			r.fieldsJSON, r.permsJSON, r.created, r.updated); err != nil {
			return nil, fmt.Errorf("upgrade: write doctype %s: %w", r.id, err)
		}
	}
	return moved, nil
}

// upgradeDocuments copies every document under its DocType's new address.
//
// A document whose org has no stored DocType row still has a home: an always-on
// lane's schema is virtual and never had a row, and an org can outlive a
// definition. Those resolve through the registry (claimed), which is the one
// place a name is matched without a module and the only place it is safe —
// the old data offers nothing else to match on, and a name two lanes both claim
// is left alone rather than guessed at.
func (s *Store) upgradeDocuments(ctx context.Context, tx *sql.Tx, moved map[[2]string]doctype.ID) error {
	rows, err := tx.QueryContext(ctx, `SELECT org,doctype,name,docstatus,data,created_at,updated_at FROM fw_documents`)
	if err != nil {
		return fmt.Errorf("upgrade: read documents: %w", err)
	}
	type row struct {
		org, name        string
		id               doctype.ID
		status           int
		data             string
		created, updated int64
	}
	var kept []row
	for rows.Next() {
		var (
			r   row
			old string
		)
		if err := rows.Scan(&r.org, &old, &r.name, &r.status, &r.data, &r.created, &r.updated); err != nil {
			_ = rows.Close()
			return fmt.Errorf("upgrade: scan document: %w", err)
		}
		id, ok := moved[[2]string{r.org, old}]
		if !ok {
			id, ok = claimed(old)
		}
		if !ok {
			// The DocType is gone and no lane claims the name. The row is already
			// unreachable; carrying it forward under an empty module keeps it
			// readable by hand rather than deleting somebody's data on a guess.
			id = doctype.ID{Name: old}
		}
		r.id = id
		kept = append(kept, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, r := range kept {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO fw_documents_next (org,module,doctype,name,docstatus,data,created_at,updated_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			r.org, r.id.Module, r.id.Name, r.name, r.status, r.data, r.created, r.updated); err != nil {
			return fmt.Errorf("upgrade: write document %s/%s: %w", r.id, r.name, err)
		}
	}
	return nil
}

// kind is the rename: the bare kind a stored name became within its module.
// Unprefixed when the module already declares that name, the tail after the
// first dash when the module declares THAT, and otherwise unchanged — an org's
// own DocType is nobody's fixture and keeps the name its owner chose.
func kind(module, name string) string {
	if declares(module, name) {
		return name
	}
	if _, tail, ok := strings.Cut(name, "-"); ok && declares(module, tail) {
		return tail
	}
	return name
}

// declares reports that a module's fixtures include this name.
func declares(module, name string) bool {
	for _, dt := range doctype.Fixtures(module) {
		if dt.Name == name {
			return true
		}
	}
	return false
}

// retarget rewrites the Link and Table targets inside a stored fields blob from
// bare names to addresses. A target is resolved in the DECLARING module first,
// then across the registry, and left alone when nothing claims it — an
// unresolvable target then fails validation on the next write, which is where a
// broken reference should become visible.
func retarget(module, fieldsJSON string) string {
	var fields []doctype.DocField
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
		return fieldsJSON
	}
	changed := false
	for i, f := range fields {
		if f.Fieldtype != doctype.FieldLink && f.Fieldtype != doctype.FieldTable {
			continue
		}
		if _, err := doctype.ParseID(f.Options); err == nil {
			continue // already an address
		}
		if k := kind(module, f.Options); declares(module, k) {
			fields[i].Options, changed = doctype.ID{Module: module, Name: k}.String(), true
			continue
		}
		if id, ok := claimed(f.Options); ok {
			fields[i].Options, changed = id.String(), true
		}
	}
	if !changed {
		return fieldsJSON
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return fieldsJSON
	}
	return string(out)
}

// claimed resolves a stored name across every registered lane. Unique or
// nothing: two lanes claiming one name is exactly the ambiguity the address
// removes, and guessing between them would hand an org the other lane's schema.
func claimed(name string) (doctype.ID, bool) {
	var (
		got doctype.ID
		n   int
	)
	for _, m := range doctype.RegisteredModules() {
		if k := kind(m, name); declares(m, k) {
			got, n = doctype.ID{Module: m, Name: k}, n+1
		}
	}
	return got, n == 1
}
