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
| `changes.go`    | the CHANGE FEED: one durable, ordered, resumable log of every committed write |
| `presence.go`   | who is viewing a document — a TTL row, announced on the change feed |
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

## The change feed — realtime

There is ONE way anything learns that a document changed, and it works for every
DocType including ones defined at runtime. Every committed write appends one row
to `fw_changes` **inside the same transaction as the write**, so a change row
exists if and only if the write landed.

```go
cursor, _ := e.ChangeCursor(ctx, caller)          // "from now on"
feed, _  := e.Changes(ctx, caller, framework.ChangesQuery{
    Since:   cursor,
    Modules: []string{"help"},                    // or DocTypes: []string{"HD Ticket"}
})
for _, c := range feed.Changes {
    // c = {Seq, DocType, Module, Name, Action, DocStatus, At}
}
cursor = feed.Cursor
```

A `Change` is a FACT and carries **no payload**. A subscriber that cares reads
the document back through the ordinary permission-checked `GetDocument`, so the
feed never becomes a second, weaker read path.

| Property | How |
|----------|-----|
| Ordered | `seq` is one `AUTOINCREMENT` over the whole table — a total order in commit order |
| Resumable | ask for `> Cursor`; `sqlite_sequence` keeps the high-water so seq never repeats after a trim |
| Scoped | the caller's validated `Org`, then the DocTypes that caller may `read` (the same `doctype.Grants` calculus) |
| Live filters | the visible set is recomputed per page, so a new DocType or a new role grant lands on the next page, not the next reconnect |
| Honest gaps | a cursor behind `Retention` (24h) gets `ChangeFeed.Reset` — never a silent gap |
| Bounded | trimmed by age at most once per minute, not once per append |

`Cursor` advances past rows the caller could not see, so a subscriber to a quiet
DocType in a busy org does not rescan the same range forever.

**Fan-out is the log, not a message.** Any process holding this database serves
any subscriber by reading rows `> cursor`. `WatchChanges()` returns a coalescing
wake channel that fires after a commit **in this process** — it removes latency,
never adds delivery. A subscriber elsewhere still sees every change on its next
read, so correctness never depends on a message reaching anybody and there is no
bus to run.

## Presence

Presence is not a second mechanism. `Announce`/`Depart` write a TTL row to
`fw_presence` (shared store, so every replica sees the whole room) and append a
`present`/`away` change **to the same feed**, naming the watched document. A
subscriber already tailing that DocType learns with no new subscription — and,
because the feed filters on read rights, cannot learn who is looking at
something it may not read. A refresh of an existing presence appends nothing, so
a busy room is silent. A viewer that crashes is forgotten when its lease lapses.

The change row carries no roster — `Present(ctx, caller, doctype, name)` does.
Same rule as documents: the feed says WHAT changed, the client re-reads the value.

## Known scope boundaries

- Schema changes (`ReplaceDocType`) do not retro-validate existing documents.
- Reverse-link integrity on delete is not enforced (forward Link validation at
  write is).
- `after_save` and multi-document hook writes are not atomic with the primary
  write — deliberate, to avoid the single-connection deadlock.
- Numeric field values must arrive as `float64` or string (the JSON shape). An
  in-process caller passing a Go `int` for a Float/Currency field is rejected.
- A `Change` records WHAT changed, not WHO changed it. Threading the actor would
  mean passing `Access` (not just `org`) through five public `Store` methods that
  hooks also call; the actor is instead a document field a lane defines.

## Compatibility

Go module rules apply: this stays at `v0.x`/`v1.x` forever. Never `v2`.
