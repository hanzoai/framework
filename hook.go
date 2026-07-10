package framework

import (
	"context"
	"sync"

	luxlog "github.com/luxfi/log"
)

// Hooks — the DocType lifecycle extension contract.
//
// A sibling app lane (CMS, ERP, CRM, Helpdesk) attaches server-side behavior to a
// DocType by registering a Go Hook against a (doctype, action) pair at process
// init. This is the pure-Go path, live now. A gpython/goja SCRIPT runner is a
// LATER, orthogonal add that registers a Hook closure implementing THIS SAME
// interface — the engine never grows a second hook path. That is the whole point
// of defining the interface now: one seam, many implementations.
//
// # Trust & scope
//
// A Hook is trusted FIRST-PARTY Go compiled into the binary — like the engine's
// own logic, inside the trust boundary. It is keyed by doctype NAME and applied
// per-org: the Event carries the VALIDATED org (never a client header), and any
// sibling data a hook reads/writes through ev.Store MUST be scoped with ev.Org, so
// a hook stays in its tenant's lane. (When the script runner lands, untrusted
// scripts run sandboxed and org-pinned — that isolation is designed there, not
// here.)
//
// # When hooks run (deadlock-safe, gate-style)
//
// Hooks run OUTSIDE the store's write transaction. The store uses a single SQLite
// connection (MaxOpenConns(1)); running a hook that touches ev.Store inside an
// open transaction would deadlock on that one connection. So the service
// orchestrates phases and the store performs each state change as its own atomic
// statement:
//
//	create:  validate → BeforeInsert → BeforeSave → INSERT → AfterSave
//	update:  validate → BeforeSave   → UPDATE → AfterSave
//	submit:  load(0)  → OnSubmit(gate) → docstatus 0→1
//	cancel:  load(1)  → OnCancel(gate) → docstatus 1→2
//	delete:  load     → OnTrash(gate)  → DELETE
//
// A gate hook (BeforeInsert/BeforeSave/OnSubmit/OnCancel/OnTrash) that returns a
// non-nil error ABORTS the operation BEFORE the state change — nothing is written
// (mapped to HTTP 422). BeforeInsert/BeforeSave may MUTATE ev.Doc.Data (e.g.
// compute a total); the mutated document is what gets persisted. AfterSave runs
// after the row is written; its error is surfaced but the write already landed.
const (
	ActionBeforeInsert = "before_insert"
	ActionBeforeSave   = "before_save" // runs on BOTH create and update
	ActionAfterSave    = "after_save"
	ActionOnSubmit     = "on_submit"
	ActionOnCancel     = "on_cancel"
	ActionOnTrash      = "on_trash" // before delete
)

// Event is the value that flows through a lifecycle Hook.
type Event struct {
	// Action is one of the Action* constants.
	Action string
	// Org is the VALIDATED tenant (clients/principal.Org). A hook that touches
	// sibling data MUST scope every query by it.
	Org string
	// DocType is the document's DocType name.
	DocType string
	// Doc is the document being acted on. In BeforeInsert/BeforeSave a hook may
	// mutate Doc.Data and the mutation is persisted; elsewhere treat it as read.
	Doc *Document
	// Prev is the previous persisted state on update/submit/cancel; nil on insert.
	Prev *Document
	// Meta is the document's DocType definition (fields, perms, flags).
	Meta *DocType
	// Store is in-org data access for hooks that read or write sibling documents.
	Store *Store
	// Logger is the framework subsystem logger.
	Logger luxlog.Logger
}

// Hook is a server-side lifecycle handler. Returning a non-nil error from a gate
// phase aborts the operation (HTTP 422) before any state change.
type Hook func(ctx context.Context, ev *Event) error

// hookRegistry maps "doctype\x00action" → ordered hooks. It is populated at
// process init (RegisterHook) and read-only during serving, guarded by a mutex so
// a late init() registration is still safe.
var (
	hookMu       sync.RWMutex
	hookRegistry = map[string][]Hook{}
)

func hookKey(doctype, action string) string { return doctype + "\x00" + action }

// RegisterHook attaches fn to (doctype, action). Multiple hooks for the same key
// run in registration order; the first error aborts the operation. Register from
// a package init() so the wiring is declared once at build time. This is the ONE
// way a DocType gains server-side behavior.
func RegisterHook(doctype, action string, fn Hook) {
	if fn == nil || doctype == "" || action == "" {
		return
	}
	hookMu.Lock()
	defer hookMu.Unlock()
	k := hookKey(doctype, action)
	hookRegistry[k] = append(hookRegistry[k], fn)
}

// runHooks invokes every hook registered for (doctype, action) in order,
// stopping at the first error. No registered hooks is a no-op.
func runHooks(ctx context.Context, action string, ev *Event) error {
	hookMu.RLock()
	hooks := hookRegistry[hookKey(ev.DocType, action)]
	hookMu.RUnlock()
	if len(hooks) == 0 {
		return nil
	}
	ev.Action = action
	for _, fn := range hooks {
		if err := fn(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

// resetHooks clears the registry. TEST-ONLY: keeps hook-behavior tests
// independent of process-global registrations.
func resetHooks() {
	hookMu.Lock()
	defer hookMu.Unlock()
	hookRegistry = map[string][]Hook{}
}
