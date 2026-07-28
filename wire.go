package framework

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/hanzoai/doctype"
)

// wire.go turns documents into transport-ready values and untrusted query
// strings into validated list options. Both directions are pure functions of
// values — no request object reaches the engine.

// Wire renders the document for transport: its field data overlaid with the
// managed envelope keys (name, doctype, docstatus, createdAt, updatedAt).
//
// Password fields are REDACTED here. This is the ONE choke point every document
// response passes through, so an argon2 hash never leaves the process: a field
// that is set becomes the fixed marker, an unset one is omitted. A client can
// learn THAT a secret is set, never anything crackable.
//
// `fields`, when non-nil, projects the data to the requested field set; the
// envelope keys are always included.
func (d Doc) Wire(fields []string) map[string]any {
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
	if d.Meta != nil {
		for _, f := range d.Meta.Fields {
			if f.Fieldtype != doctype.FieldPassword {
				continue
			}
			if v, ok := m[f.Fieldname]; ok {
				if s, _ := v.(string); s != "" {
					m[f.Fieldname] = doctype.RedactedMarker
				} else {
					delete(m, f.Fieldname)
				}
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

// WireAll renders a list of documents.
func WireAll(docs []Doc, fields []string) []map[string]any {
	out := make([]map[string]any, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Wire(fields))
	}
	return out
}

// ListQuery is a list request exactly as a transport received it: raw,
// untrusted strings. ParseListQuery validates it against a DocType schema.
// Keeping the raw shape here means a host does no parsing of its own and
// therefore cannot parse it differently.
type ListQuery struct {
	// Filters is a JSON object of equality matches, e.g. `{"status":"Paid"}`.
	Filters string
	// Fields projects the response: a JSON array `["a","b"]` or a comma list "a,b".
	Fields string
	// OrderBy is `<field> [asc|desc]`. Empty means most-recently-updated first.
	OrderBy string
	// Limit is a positive integer; out-of-range falls back to the default.
	Limit string
}

// ParseListQuery validates a raw list query against a DocType, returning the
// store options and the requested field projection.
//
// Filter and sort field names MUST be declared on the DocType (or the managed
// name/docstatus), and every value is carried as a bound parameter into the
// store — a name that is not in the schema is refused rather than reaching a
// JSON path. An unknown field is a validation error, not a silent no-op.
func ParseListQuery(dt *doctype.DocType, q ListQuery) (ListOpts, []string, error) {
	opts := ListOpts{Limit: doctype.DefaultLimit}

	if raw := strings.TrimSpace(q.Filters); raw != "" {
		var f map[string]any
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			return opts, nil, doctype.Errorf("filters must be a JSON object")
		}
		opts.Filters = make(map[string]string, len(f))
		for k, v := range f {
			if !queryable(dt, k) {
				return opts, nil, doctype.Errorf("unknown filter field: %s", k)
			}
			opts.Filters[k] = fmt.Sprint(v)
		}
	}

	var fields []string
	if raw := strings.TrimSpace(q.Fields); raw != "" {
		for _, f := range splitFields(raw) {
			if _, ok := dt.Field(f); !ok {
				return opts, nil, doctype.Errorf("unknown field: %s", f)
			}
			fields = append(fields, f)
		}
	}

	if raw := strings.TrimSpace(q.OrderBy); raw != "" {
		parts := strings.Fields(raw)
		field := parts[0]
		if !queryable(dt, field) {
			return opts, nil, doctype.Errorf("unknown order_by field: %s", field)
		}
		opts.OrderField = field
		if len(parts) > 1 && strings.EqualFold(parts[1], "desc") {
			opts.Desc = true
		}
	} else {
		opts.Desc = true // default: most-recently-updated first
	}

	if raw := strings.TrimSpace(q.Limit); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			opts.Limit = n
		}
	}
	return opts, fields, nil
}

// queryable reports whether a name may be filtered or sorted on: a declared
// field, or the managed columns name/docstatus.
func queryable(dt *doctype.DocType, name string) bool {
	if name == "name" || name == "docstatus" {
		return true
	}
	_, ok := dt.Field(name)
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

// BindDocument decodes a document body with the engine's size guard. A host
// hands it the raw bytes it received; the bound is the engine's, so every host
// enforces the same one.
func BindDocument(body []byte) (map[string]any, error) {
	if len(body) > doctype.MaxDocBytes {
		return nil, doctype.Errorf("document body too large")
	}
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, doctype.Errorf("invalid JSON body")
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}
