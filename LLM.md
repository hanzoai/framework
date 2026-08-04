# hanzoai/framework — the DocType engine

`github.com/hanzoai/framework` is the metadata-driven **DocType engine**: native
Go on SQLite, Frappe's document engine rebuilt with **no Frappe/Python runtime**.

It is the FOUNDATION for business apps. CMS content-types, ERPNext DocTypes, CRM
records and Helpdesk tickets are all **just DocTypes** on this one engine, so ONE
engine plus ONE generic metadata-driven renderer covers every app lane. A lane
defines its DocTypes and attaches behaviour through the Go hook interface.
**Do not fork the engine — extend it through DocTypes and hooks.**

## Layering

```
doctype     the VALUE  — schema, coercion, naming, permission calculus. No I/O.
framework   the ENGINE — this module: store, validation-against-data, role
                         resolution, hooks, leases, operations. No transport.
host        the PLACE  — an HTTP service, a CLI, a job runner. Authenticates
                         the caller; adapts operations to its own transport.
```

The engine does not know what HTTP is, where its master key comes from, or how a
user was authenticated. Those are the host's decisions, injected through
`Config` and `Caller`. Hanzo Cloud is one host (mounting `/v1/framework/*`); it
is not a privileged one.

## Files

| File | Responsibility |
|------|----------------|
| `engine.go`     | `Engine`, `Config`, `Caller`, `OpenDBFunc` — construction and the injected seams |
| `ops.go`        | the OPERATION set: DocType registry, roles, modules, document CRUD + lifecycle |
| `store.go`      | per-org SQLite store (doctypes, documents, series, roles, locks); generic CRUD |
| `validate.go`   | the half of validation that reads tenant data: Link, Unique, child tables, fetch-from |
| `permission.go` | role RESOLUTION, the manager gate, trust-on-first-use owner seeding |
| `hook.go`       | the Go lifecycle hook interface + registry |
| `lock.go`       | the store lease: an exclusive, TTL-bounded, cross-process interlock |
| `ingest.go`     | the in-process API for first-party producers (`Ingest`/`Get`/`Search`/…) |
| `wire.go`       | `Doc.Wire` (the redaction choke point) + `ParseListQuery` + `BindDocument` |
| `errors.go`     | sentinels, `HookAbort`, and `Classify` — what a failure MEANS |
| `doctype.go`    | re-exports the value vocabulary as aliases, so a lane needs ONE import |

## The two injected seams

Everything the engine used to take from its host is now one of these.

```go
// Storage policy belongs to the host. Hanzo Cloud injects an encrypted-at-rest
// opener (KMS-held master key); a standalone app or test leaves it nil and gets
// the plain hanzoai/sqlite driver. The engine never decides encryption — it
// could only ever guess wrong.
type OpenDBFunc func(path string) (*sql.DB, error)

// Authentication belongs to the host. Org is the VALIDATED tenant, used
// verbatim and scoped onto every query, so it MUST come from a verified
// credential — never a client-supplied header. A Caller with an empty Org is
// refused before any store access.
type Caller struct {
    Org     string
    User    string
    IsAdmin bool // PLATFORM superuser, not an org admin
}
```

`Caller` is a value, not a resolver interface, deliberately: the engine needs
the tenant, not the ability to go looking for one. It cannot be handed a request
to inspect, so there is exactly ONE way a tenant enters the engine.

## Operations

Every operation takes a `Caller` and **enforces permissions itself**, so
authorization cannot be forgotten by a host and is not re-implemented by each
one. A host's job is: decode, resolve a Caller, call, render, map the Code.

```go
e, _ := framework.Open(framework.Config{Dir: dataDir})
defer e.Close()

doc, err := e.CreateDocument(ctx, caller, "Sales Invoice", map[string]any{
    "customer": "Widgets Ltd",
})
switch framework.Classify(err) {
case framework.CodeForbidden: // 403
case framework.CodeNotFound:  // 404
case framework.CodeInvalid:   // 400
case framework.CodeConflict:  // 409
case framework.CodeRejected:  // 422 — dangling Link, or a gate hook vetoed it
}
render(doc.Wire(nil))
```

`Classify` is the ONE place that decides what a failure means. A host switches
on the `Code` and never on a message, so hosts cannot drift.

## Tenant isolation (the security boundary)

Every `fw_*` table carries an `org` column and every query filters `WHERE org=?`,
so org A's schema and data are physically invisible to org B. The org always
comes from `Caller.Org`. There is exactly one org-derivation path.

## Permissions — secure by default

- **Platform superuser** (`Caller.IsAdmin`) is a manager everywhere.
- **Owner seeding (trust-on-first-use)**: the FIRST validated principal to
  administer an org with no role assignments becomes its System Manager,
  persisted ONCE via a single conditional INSERT — so "exactly one" holds under
  concurrency. Every later member has no privilege until granted.
- **Default-closed**: a DocType is NEVER open to all. There is no "empty perms
  means open" path; enforcement fails closed for a role-less member.

## Password fields

Hashed argon2id on write, redacted on read. `Doc.Wire` is the ONE choke point
every document response passes through, which is why operations return a `Doc`
(document **plus** its schema) rather than a bare document — handing back a bare
document would let a host marshal it directly and leak the hash.

## Hooks

Attach behaviour by registering a `Hook` from a package `init()`:

```go
framework.RegisterHook("Sales Invoice", framework.ActionBeforeSave,
    func(ctx context.Context, ev *framework.Event) error {
        ev.Doc.Data["total"] = computeTotal(ev.Doc.Data)  // mutation persists
        return nil
    })
```

| Action | When | Abort? |
|--------|------|--------|
| `ActionBeforeInsert` | create only, before naming/insert | yes |
| `ActionBeforeSave`   | create AND update, before write | yes (may mutate `Doc.Data`) |
| `ActionAfterSave`    | after the row is written | logged, not fatal |
| `ActionOnSubmit`     | before docstatus 0→1 | yes |
| `ActionOnCancel`     | before docstatus 1→2 | yes |
| `ActionOnTrash`      | before delete | yes |

Hooks run **outside** the store transaction (the store holds a single SQLite
connection; a hook touching `ev.Store` inside an open tx would deadlock). Gate
hooks therefore run BEFORE the state change: an error aborts the operation
before anything is written, and the engine wraps it as a `*HookAbort` so a host
can tell a refusal from a fault.

## Known scope boundaries

- Schema changes (`ReplaceDocType`) do not retro-validate existing documents.
- Reverse-link integrity on delete is not enforced (forward Link validation at
  write is).
- `after_save` and multi-document hook writes are not atomic with the primary
  write — deliberate, to avoid the single-connection deadlock.
- Numeric field values must arrive as `float64` or string (the JSON shape). An
  in-process caller passing a Go `int` for a Float/Currency field is rejected.

## Compatibility

Go module rules apply: this stays at `v0.x`/`v1.x` forever. Never `v2`.

## Provenance

Original Hanzo work. This engine rebuilds Frappe's DocType **concept** in Go
from its documented behaviour — the metadata model, the `docstatus` lifecycle,
the autoname grammar, the role/perm vocabulary. No Frappe code was copied,
ported or translated, and nothing in the dependency graph derives from Frappe.
The negative fingerprints are cheap to re-check: tables are `fw_*` where
Frappe's are `tab*`; our `DocField` carries 11 properties against the 80 in
Frappe's `docfield.json`; and the ones we do share are spelled camelCase
(`inListView`, `fetchFrom`, `readOnly`) where Frappe writes `in_list_view`,
`fetch_from`, `read_only` — a translated port would have carried the
snake_case through. No Frappe error string or column name appears anywhere in
the tree.

What IS shared is interface, on purpose: `INV-.YYYY.-.#####` and `System
Manager` mean here what they mean there, so anyone who knows Frappe can read
this schema. A shared vocabulary is not shared code.

Apache 2.0 is therefore Hanzo's own grant over its own new code, inherited from
no one. `NOTICE` exists because we author it — §4(d) obliges a NOTICE only where
an upstream work shipped one, and there is no upstream. **Do not add Frappe
attribution here**: Frappe is MIT, but we took none of its code, and claiming
otherwise would misstate the provenance.
