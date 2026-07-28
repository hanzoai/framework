package framework

import (
	"context"
	"fmt"
	"strings"

	"github.com/hanzoai/doctype"
)

// validate.go is the half of document validation that must READ tenant data:
// Link targets must exist IN THE ORG, Unique values must not already be taken,
// child Table rows must validate against their child DocType, and fetch-from
// fields must be populated from the linked document.
//
// The other half — coercing a raw JSON value to its fieldtype — is pure and
// lives in doctype.Coerce, which this calls for every scalar. The split is
// exactly "does answering this question require the database": if not, it is a
// value computation and belongs in the doctype module, where a renderer or a
// linter can reach it without linking a store.

// validateDoc validates an incoming document body `in` against a DocType schema
// and returns the CANONICAL data map to persist: only declared fields, each
// coerced/checked by its fieldtype, Password fields hashed, Link fields verified
// in-org, Table rows validated against their child DocType, fetch-from fields
// populated. Unknown input keys are ignored (no junk reaches storage). `prev` is
// the previously-stored data on update (nil on create) — used to PRESERVE a
// Password whose input is the redacted marker or an already-hashed value.
//
// It is a *Store method because Link/Table/Unique checks read the org's data; it
// runs BEFORE any write transaction (single-connection safe) and never mutates
// storage. `excludeName` is the name of the document being updated, so a Unique
// check skips its own row (empty on create). The `child` flag validates a Table
// child row: Unique and fetch-from do not apply and a nested Table is rejected
// (one level of child tables).
func (s *Store) validateDoc(ctx context.Context, org string, dt *DocType, in, prev map[string]any, excludeName string, child bool) (map[string]any, error) {
	out := make(map[string]any, len(dt.Fields))

	for _, f := range dt.Fields {
		raw, present := in[f.Fieldname]
		if child && f.Fieldtype == FieldTable {
			return nil, doctype.Errorf("field %q: nested child tables are not supported", f.Fieldname)
		}

		// Password preservation: a redacted marker / already-hashed / absent value
		// on update keeps the prior hash; it is never written as plaintext.
		if f.Fieldtype == FieldPassword {
			if v, ok := doctype.PasswordValue(raw, present); ok {
				h, err := doctype.HashPassword(v)
				if err != nil {
					return nil, err
				}
				out[f.Fieldname] = h
			} else if prev != nil {
				if old, ok := prev[f.Fieldname]; ok {
					out[f.Fieldname] = old
				}
			}
			if _, set := out[f.Fieldname]; !set && f.Reqd {
				return nil, doctype.Errorf("field %q is mandatory", f.Fieldname)
			}
			continue
		}

		// Default when absent/empty.
		if doctype.IsEmptyInput(raw, present) {
			if f.Default != "" {
				raw, present = f.Default, true
			}
		}
		if doctype.IsEmptyInput(raw, present) {
			if f.Reqd {
				return nil, doctype.Errorf("field %q is mandatory", f.Fieldname)
			}
			continue
		}

		val, err := doctype.Coerce(f, raw)
		if err != nil {
			return nil, err
		}

		switch f.Fieldtype {
		case FieldLink:
			name, _ := val.(string)
			ok, err := s.documentExists(ctx, org, f.Options, name)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, ErrBadRef
			}
		case FieldTable:
			rows, err := s.validateChildTable(ctx, org, f, val)
			if err != nil {
				return nil, err
			}
			val = rows
		}

		if f.Unique && !child {
			if str := fmt.Sprint(val); str != "" {
				taken, err := s.fieldValueTaken(ctx, org, dt.Name, f.Fieldname, str, excludeName)
				if err != nil {
					return nil, err
				}
				if taken {
					return nil, doctype.Errorf("field %q must be unique (value already exists)", f.Fieldname)
				}
			}
		}

		out[f.Fieldname] = val
	}

	if !child {
		if err := s.applyFetchFrom(ctx, org, dt, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// validateChildTable validates a Table field's rows against the child DocType
// named by field.Options, returning the normalized []map[string]any to embed in
// the parent document. Child rows are components of the parent (Frappe child
// tables), stored inline — atomic with the parent.
func (s *Store) validateChildTable(ctx context.Context, org string, f DocField, val any) ([]map[string]any, error) {
	arr, ok := val.([]any)
	if !ok {
		return nil, doctype.Errorf("field %q (Table) must be an array", f.Fieldname)
	}
	if len(arr) > doctype.MaxChildRows {
		return nil, doctype.Errorf("field %q has too many rows (max %d)", f.Fieldname, doctype.MaxChildRows)
	}
	childDT, err := s.GetDocType(ctx, org, f.Options)
	if err == ErrNotFound {
		return nil, doctype.Errorf("field %q references unknown child doctype %q", f.Fieldname, f.Options)
	}
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(arr))
	for i, r := range arr {
		row, ok := r.(map[string]any)
		if !ok {
			return nil, doctype.Errorf("field %q row %d must be an object", f.Fieldname, i)
		}
		validated, err := s.validateDoc(ctx, org, &childDT, row, nil, "", true)
		if err != nil {
			return nil, err
		}
		out = append(out, validated)
	}
	return out, nil
}

// applyFetchFrom populates fetch-from fields from their linked documents. For a
// field with FetchFrom "link_field.source_field", when link_field has a value the
// engine loads that document (in-org) and copies source_field into this field.
func (s *Store) applyFetchFrom(ctx context.Context, org string, dt *DocType, out map[string]any) error {
	for _, f := range dt.Fields {
		if f.FetchFrom == "" {
			continue
		}
		linkName, src, _ := strings.Cut(f.FetchFrom, ".")
		linkVal, _ := out[linkName].(string)
		if linkVal == "" {
			continue
		}
		lf, ok := dt.Field(linkName)
		if !ok || lf.Fieldtype != FieldLink {
			continue
		}
		// Never fetch a secret: if the source field is a Password in the target
		// DocType, its stored value is an argon2 hash — copying it into this
		// (non-Password) field would leak the hash past wireDoc's redaction.
		if target, err := s.GetDocType(ctx, org, lf.Options); err == nil {
			if sf, ok := target.Field(src); ok && sf.Fieldtype == FieldPassword {
				continue
			}
		}
		doc, err := s.GetDocument(ctx, org, lf.Options, linkVal)
		if err == ErrNotFound {
			continue
		}
		if err != nil {
			return err
		}
		if v, ok := doc.Data[src]; ok {
			out[f.Fieldname] = v
		}
	}
	return nil
}
