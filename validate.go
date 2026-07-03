package framework

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// validationError is a document-schema violation (400). It is distinct from the
// store sentinels (errBadRef → 422, errConflict → 409) so the HTTP layer answers
// the right code. Detect with errors.As.
type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }

func validationErrorf(format string, args ...any) error {
	return &validationError{msg: fmt.Sprintf(format, args...)}
}

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
			return nil, validationErrorf("field %q: nested child tables are not supported", f.Fieldname)
		}

		// Password preservation: a redacted marker / already-hashed / absent value
		// on update keeps the prior hash; it is never written as plaintext.
		if f.Fieldtype == FieldPassword {
			if v, ok := passwordValue(raw, present); ok {
				h, err := hashPassword(v)
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
				return nil, validationErrorf("field %q is mandatory", f.Fieldname)
			}
			continue
		}

		// Default when absent/empty.
		if isEmptyInput(raw, present) {
			if f.Default != "" {
				raw, present = f.Default, true
			}
		}
		if isEmptyInput(raw, present) {
			if f.Reqd {
				return nil, validationErrorf("field %q is mandatory", f.Fieldname)
			}
			continue
		}

		val, err := coerceField(f, raw)
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
				return nil, errBadRef
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
					return nil, validationErrorf("field %q must be unique (value already exists)", f.Fieldname)
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
		return nil, validationErrorf("field %q (Table) must be an array", f.Fieldname)
	}
	if len(arr) > maxChildRows {
		return nil, validationErrorf("field %q has too many rows (max %d)", f.Fieldname, maxChildRows)
	}
	childDT, err := s.GetDocType(ctx, org, f.Options)
	if err == errNotFound {
		return nil, validationErrorf("field %q references unknown child doctype %q", f.Fieldname, f.Options)
	}
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(arr))
	for i, r := range arr {
		row, ok := r.(map[string]any)
		if !ok {
			return nil, validationErrorf("field %q row %d must be an object", f.Fieldname, i)
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
		lf, ok := dt.field(linkName)
		if !ok || lf.Fieldtype != FieldLink {
			continue
		}
		// Never fetch a secret: if the source field is a Password in the target
		// DocType, its stored value is an argon2 hash — copying it into this
		// (non-Password) field would leak the hash past wireDoc's redaction.
		if target, err := s.GetDocType(ctx, org, lf.Options); err == nil {
			if sf, ok := target.field(src); ok && sf.Fieldtype == FieldPassword {
				continue
			}
		}
		doc, err := s.GetDocument(ctx, org, lf.Options, linkVal)
		if err == errNotFound {
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

// coerceField converts a raw JSON value to the canonical stored value for a
// fieldtype, or returns a validationError. This is the pure (DB-free) core of
// field-type validation — the gate RED must not be able to bypass.
func coerceField(f DocField, raw any) (any, error) {
	switch f.Fieldtype {
	case FieldData, FieldText, FieldSmall, FieldLong, FieldAttach:
		s, err := asString(f, raw)
		if err != nil {
			return nil, err
		}
		return clip(s), nil

	case FieldSelect:
		s, err := asString(f, raw)
		if err != nil {
			return nil, err
		}
		s = strings.TrimSpace(s)
		for _, choice := range f.selectChoices() {
			if s == strings.TrimSpace(choice) {
				return s, nil
			}
		}
		return nil, validationErrorf("field %q: %q is not an allowed option", f.Fieldname, s)

	case FieldLink:
		s, err := asString(f, raw)
		if err != nil {
			return nil, err
		}
		return strings.TrimSpace(s), nil

	case FieldInt:
		return asInt64(f, raw)

	case FieldFloat, FieldCurrency:
		return asFloat64(f, raw)

	case FieldCheck:
		return asCheck(f, raw)

	case FieldDate:
		return asDate(f, raw, "2006-01-02", "date (YYYY-MM-DD)")

	case FieldDatetime:
		return asDatetime(f, raw)

	case FieldJSON:
		// Stored as-is; it is already valid JSON (it was unmarshaled from the body).
		return raw, nil

	case FieldTable:
		return raw, nil // shape-validated by validateChildTable

	default:
		return nil, validationErrorf("field %q: unsupported fieldtype %q", f.Fieldname, f.Fieldtype)
	}
}

// ---- typed coercions ----

func asString(f DocField, raw any) (string, error) {
	s, ok := raw.(string)
	if !ok {
		return "", validationErrorf("field %q expects text", f.Fieldname)
	}
	return s, nil
}

func asInt64(f DocField, raw any) (int64, error) {
	switch v := raw.(type) {
	case float64:
		if v != float64(int64(v)) {
			return 0, validationErrorf("field %q expects an integer", f.Fieldname)
		}
		return int64(v), nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, validationErrorf("field %q expects an integer", f.Fieldname)
		}
		return n, nil
	default:
		return 0, validationErrorf("field %q expects an integer", f.Fieldname)
	}
}

func asFloat64(f DocField, raw any) (float64, error) {
	switch v := raw.(type) {
	case float64:
		return v, nil
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, validationErrorf("field %q expects a number", f.Fieldname)
		}
		return n, nil
	default:
		return 0, validationErrorf("field %q expects a number", f.Fieldname)
	}
}

// asCheck normalizes to 0/1. A Check is stored as an integer so json_extract
// filters compare cleanly.
func asCheck(f DocField, raw any) (int, error) {
	switch v := raw.(type) {
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	case float64:
		if v == 0 {
			return 0, nil
		}
		return 1, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return 1, nil
		case "0", "false", "no", "off", "":
			return 0, nil
		}
	}
	return 0, validationErrorf("field %q expects a boolean/check", f.Fieldname)
}

func asDate(f DocField, raw any, layout, label string) (string, error) {
	s, err := asString(f, raw)
	if err != nil {
		return "", err
	}
	s = strings.TrimSpace(s)
	if _, err := time.Parse(layout, s); err != nil {
		return "", validationErrorf("field %q expects a %s", f.Fieldname, label)
	}
	return s, nil
}

// asDatetime accepts "YYYY-MM-DD HH:MM:SS" or RFC3339 and canonicalizes to the
// former.
func asDatetime(f DocField, raw any) (string, error) {
	s, err := asString(f, raw)
	if err != nil {
		return "", err
	}
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02 15:04:05"), nil
		}
	}
	return "", validationErrorf("field %q expects a datetime (YYYY-MM-DD HH:MM:SS)", f.Fieldname)
}

// passwordValue reports the plaintext to hash for a Password field, and false
// when the input should PRESERVE the prior value (absent, empty, the redacted
// marker, or an already-hashed value — none of which is a new plaintext).
func passwordValue(raw any, present bool) (string, bool) {
	if !present {
		return "", false
	}
	s, ok := raw.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" || s == redactedMarker || isHashed(s) {
		return "", false
	}
	return s, true
}

// isEmptyInput reports whether a raw input is absent or an empty string.
func isEmptyInput(raw any, present bool) bool {
	if !present {
		return true
	}
	if s, ok := raw.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	return false
}

// clip trims and bounds a text value to maxField.
func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxField {
		return s[:maxField]
	}
	return s
}
