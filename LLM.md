# Hanzo Framework — the DocType engine contract

`clients/framework` is the metadata-driven **DocType engine**, native Go on
Base/SQLite, mounted in the unified cloud binary at `/v1/framework/*` (subsystem
order 129). It is Frappe's DocType/metadata core rebuilt in pure Go — **no
Frappe/Python runtime**. It is the FOUNDATION: CMS content-types, ERPNext
DocTypes, and Helpdesk are all **just DocTypes** on this one engine, so ONE
engine + ONE generic `@hanzo/ui` DocType renderer (list/form/detail driven by
DocType metadata) renders every business app.

Sibling app lanes (CMS/ERP/CRM/Helpdesk) build to THIS contract: they define
their DocTypes on the engine and attach behavior via the Go hook interface. Do
not fork the engine — extend it through DocTypes + hooks.

## Files

| File | Responsibility |
|------|----------------|
| `doctype.go`   | `DocType`/`DocField`/`DocPerm` types, fieldtype set, schema `Validate()` |
| `store.go`     | per-org SQLite store (doctypes, documents, series, roles); generic CRUD |
| `validate.go`  | document validation against a DocType schema (all fieldtypes) |
| `naming.go`    | autoname engine (hash / field / prompt / series patterns) |
| `secret.go`    | argon2id hashing for Password fields |
| `permission.go`| per-org role resolution + the `can(doctype, right)` gate |
| `hook.go`      | the Go lifecycle hook interface + registry |
| `framework.go` | Mount + HTTP handlers + registration |

## Tenant isolation (the security boundary)

Every request resolves its org through the ONE boundary `clients/principal.Tenant`,
which returns the org ONLY for a **validated principal** (a gateway/BFF-minted
`X-User-Id` from a verified IAM credential), used **verbatim** and cloned. A
forged `X-Org-Id` with no validated principal is refused **403** before any store
access. Every `fw_*` table carries an `org` column and every query filters
`WHERE org=?`. There is exactly ONE org-derivation path in the package.

## DocType JSON shape

```json
{
  "name": "Sales Invoice",
  "module": "Accounts",
  "isSingle": false,
  "isSubmittable": true,
  "autoname": "INV-.YYYY.-.#####",
  "titleField": "customer",
  "fields": [
    {"fieldname": "customer",  "fieldtype": "Link",     "label": "Customer", "options": "Customer", "reqd": true},
    {"fieldname": "region",    "fieldtype": "Data",     "label": "Region",   "fetchFrom": "customer.region"},
    {"fieldname": "status",    "fieldtype": "Select",   "label": "Status",   "options": "Draft\nPaid", "default": "Draft"},
    {"fieldname": "total",     "fieldtype": "Currency", "label": "Total"},
    {"fieldname": "items",     "fieldtype": "Table",    "label": "Items",    "options": "Sales Invoice Item"},
    {"fieldname": "api_secret","fieldtype": "Password", "label": "API Secret"}
  ],
  "permissions": [
    {"role": "Accounts Manager", "read": true, "write": true, "create": true, "delete": true, "submit": true, "cancel": true},
    {"role": "Accounts User",    "read": true, "create": true}
  ]
}
```

### Fieldtypes (mirror Frappe's DocField)

| Fieldtype | Stored as | Notes |
|-----------|-----------|-------|
| `Data` `Text` `SmallText` `LongText` | string | trimmed, ≤100 KB |
| `Int` | int64 | rejects non-integer |
| `Float` `Currency` | float64 | |
| `Check` | int 0/1 | accepts bool/0/1/"true" |
| `Date` | `"YYYY-MM-DD"` | format-validated |
| `Datetime` | `"YYYY-MM-DD HH:MM:SS"` | accepts RFC3339, canonicalized |
| `Select` | string | must be one of `options` (newline-separated) |
| `Link` | string | the `name` of a doc of `options` DocType; **verified in-org** at write (dangling → 422) |
| `Table` | `[]object` | child rows validated against the `options` child DocType (one level; no nested tables) |
| `Attach` | string | URL/path to media |
| `JSON` | any | stored as-is |
| `Password` | argon2id hash | **hashed on write, redacted on read** — never returned in clear or as hash (`"__set__"` marker if set). A retrievable secret belongs in KMS, not here. |

Field extras: `reqd`, `default`, `unique` (enforced in-org), `readOnly`,
`hidden`, `inListView`, `fetchFrom` (`"link_field.source_field"` — auto-populate
from a linked doc; never fetches a Password source).

### Naming (`autoname`)

- `""` / `"hash"` → random 128-bit hex id (default)
- `"field:fieldname"` → the value of that field (must be unique in org+doctype)
- `"prompt"` → the client supplies `name` in the POST body
- a **series pattern** → dot-delimited tokens: `YYYY`/`YY`/`MM`/`DD` date parts and
  a run of `#` marking a zero-padded per-org counter, e.g. `INV-.YYYY.-.#####` →
  `INV-2026-00001`. No `#` run → a default 5-digit counter is appended.

## Document CRUD (generic, metadata-driven)

The document wire shape is the field data overlaid with the managed envelope keys
`name`, `doctype`, `docstatus`, `createdAt`, `updatedAt`. On write, send field
values at the top level (plus `name` for prompt/single naming); unknown keys are
ignored.

```
GET    /v1/framework/:doctype              list   ?filters={"status":"Paid"}&fields=customer,total&order_by=modified desc&limit=50
POST   /v1/framework/:doctype              create → 201 Document
GET    /v1/framework/:doctype/:name        detail
PUT    /v1/framework/:doctype/:name        update (draft only; docstatus 0)
DELETE /v1/framework/:doctype/:name        delete (a submitted doc must be cancelled first)
POST   /v1/framework/:doctype/:name/submit docstatus 0→1  (isSubmittable)
POST   /v1/framework/:doctype/:name/cancel docstatus 1→2  (isSubmittable)
```

`filters` is a JSON object of equality matches (field names must be declared or
`name`/`docstatus`; values are bound parameters). `fields` projects the returned
data (envelope keys always included). `order_by` is `"<field> asc|desc"`.

DocType registry: `GET/POST /v1/framework/doctypes`,
`GET/PUT/DELETE /v1/framework/doctypes/:name`. Defining/replacing/deleting a
DocType requires the **System Manager** role (or global admin).

## Permissions (per-org, DocType perms by role) — secure by default

The caller's role source is the framework's OWN per-org role store (IAM's JWT
carries no roles into the cloud binary), managed at `/v1/framework/roles`
(`GET`, `POST {user,role}`, `DELETE /:user/:role`). Enforcement:

- **Global admin** (`c.IsAdmin()`, validated owner == AdminOrg) may do anything.
- **Owner seeding (trust-on-first-use)**: the FIRST validated principal to
  administer an org that has no role assignments becomes its **System Manager**,
  persisted ONCE — the org owner/creator. Every later member has no privilege
  until the owner grants a role. Never crosses a tenant (org is the validated
  tenant), and exactly one member is auto-granted (deterministically the first).
- **Default-closed**: a DocType is NEVER open to all. A DocType declared with no
  `permissions` is seeded a System Manager grant at define time and is therefore
  manager-only; a role-less member is denied. There is no "empty perms = open"
  path — the enforcement fails closed.
- Otherwise, a right (`read`/`write`/`create`/`delete`/`submit`/`cancel`) is
  granted iff one of the caller's roles carries it in the DocType's `permissions`.

This is stricter than the legacy per-org subsystems (where every org member shared
org data): the Framework is the new canonical secure-by-default way, and those
subsystems migrate onto it.

## Go hook interface (lifecycle extension)

Attach server-side behavior to a DocType by registering a `Hook` from a package
`init()`. This is the pure-Go path, live now; a gpython/goja script runner is a
LATER, orthogonal add that registers a `Hook` closure implementing THIS SAME
interface — the engine never grows a second hook path.

```go
import "github.com/hanzoai/cloud/clients/framework"

func init() {
    // Compute a total before every save.
    framework.RegisterHook("Sales Invoice", framework.ActionBeforeSave,
        func(ctx context.Context, ev *framework.Event) error {
            var total float64
            if items, ok := ev.Doc.Data["items"].([]map[string]any); ok {
                for _, it := range items {
                    if amt, ok := it["amount"].(float64); ok { total += amt }
                }
            }
            ev.Doc.Data["total"] = total        // mutation persists
            return nil
        })

    // Gate: refuse to submit an unbalanced invoice.
    framework.RegisterHook("Sales Invoice", framework.ActionOnSubmit,
        func(ctx context.Context, ev *framework.Event) error {
            if ev.Doc.Data["total"] == float64(0) {
                return fmt.Errorf("cannot submit a zero-total invoice") // → HTTP 422
            }
            return nil
        })
}
```

### Event

```go
type Event struct {
    Action  string     // Action* constant
    Org     string     // VALIDATED tenant — scope every sibling query by it
    DocType string
    Doc     *Document  // Name, DocStatus, Data (mutable in before_* phases)
    Prev    *Document  // previous state on update/submit/cancel; nil on insert
    Meta    *DocType   // the schema (fields, perms, flags)
    Store   *Store     // in-org data access for sibling reads/writes
    Logger  luxlog.Logger
}
type Hook func(ctx context.Context, ev *Event) error
```

### Actions & semantics

| Action | When | Abort? |
|--------|------|--------|
| `ActionBeforeInsert` | create only, before naming/insert | yes → 422 |
| `ActionBeforeSave`   | create AND update, before write | yes → 422 (may mutate `Doc.Data`) |
| `ActionAfterSave`    | after the row is written | logged, not fatal |
| `ActionOnSubmit`     | before docstatus 0→1 | yes → 422 |
| `ActionOnCancel`     | before docstatus 1→2 | yes → 422 |
| `ActionOnTrash`      | before delete | yes → 422 |

Hooks run **outside** the store transaction (the store uses a single SQLite
connection; a hook touching `ev.Store` inside an open tx would deadlock). Gate
hooks therefore run BEFORE the state change: returning an error aborts the
operation before anything is written. `after_save` runs post-write (the primary
document write is already atomic; cross-document atomicity is a hook's own
responsibility). Hooks are trusted first-party Go inside the trust boundary,
keyed by doctype NAME and applied per-org via `ev.Org`.

## Known scope boundaries (v1)

- Schema changes (`PUT doctype`) do not retro-validate existing documents.
- Reverse-link integrity on delete is not enforced (forward Link validation at
  write is; matches the existing per-org subsystems).
- `after_save` and multi-document hook writes are not atomic with the primary
  write (deliberate — SQLite single-connection deadlock avoidance).
