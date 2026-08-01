# Hanzo Framework

The metadata-driven DocType engine: native Go on SQLite, Frappe's document engine rebuilt
with no Frappe or Python runtime.

It is the foundation for business apps. CMS content types, ERP DocTypes, CRM records and
helpdesk tickets are all just DocTypes on this one engine, so one engine plus one generic
metadata-driven renderer covers every app. An app defines its DocTypes and attaches
behaviour through the Go hook interface — extend the engine through DocTypes and hooks,
do not fork it.

## Install

```bash
go get github.com/hanzoai/framework
```

## The layering

```
doctype     the VALUE  — schema, coercion, naming, permission calculus. No I/O.
framework   the ENGINE — this module: store, validation against data, role
                         resolution, hooks, leases, operations. No transport.
host        the PLACE  — an HTTP service, a CLI, a job runner. Authenticates the
                         caller and adapts operations to its own transport.
```

The engine does not know what HTTP is, where its master key comes from, or how a caller
was authenticated. That is the host's job, and keeping it there is what lets the same
engine serve a web service, a CLI and a job runner without changing.

## Docs

[`LLM.md`](LLM.md) is the reference — the engine's contracts, the hook interface, and what
must not change. [`hanzoai/doctype`](https://github.com/hanzoai/doctype) is the value layer
below it.

## License

See [LICENSE](LICENSE).
