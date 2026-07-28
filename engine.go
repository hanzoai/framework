// Package framework is the Hanzo DocType engine: metadata-driven document
// storage, validation, permissions and lifecycle hooks, native Go on SQLite.
// It is Frappe's document engine rebuilt in Go — no Frappe/Python runtime.
//
// It is the FOUNDATION for business apps: CMS content-types, ERPNext DocTypes,
// CRM records and Helpdesk tickets are all just DocTypes on this one engine, so
// ONE engine plus ONE generic metadata-driven renderer covers every app lane.
// A lane defines its DocTypes (github.com/hanzoai/doctype) and attaches
// behaviour through the Go hook interface. Do not fork the engine — extend it
// through DocTypes and hooks.
//
// # Layering
//
//	doctype    the VALUE layer — schema, coercion, naming, permission calculus.
//	framework  the ENGINE — this package: store, validation-against-data, role
//	           resolution, hooks, leases, operations.
//	host       the PLACE — an HTTP service, a CLI, a job runner. The host
//	           authenticates the caller and adapts the engine's operations to
//	           its own transport. The engine has no transport of its own.
//
// The engine deliberately does not know what HTTP is, where its master key
// comes from, or how a user was authenticated. Those are the host's decisions,
// injected through Config and Caller.
package framework

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	luxlog "github.com/luxfi/log"
)

// OpenDBFunc opens the engine's SQLite database.
//
// It exists so the HOST owns the storage policy. Hanzo Cloud injects its
// encrypted-at-rest opener (a KMS-held master key, per-database data key); a
// standalone business app, a test, or a CLI leaves it nil and gets the plain
// hanzoai/sqlite driver. The engine never decides whether data is encrypted —
// it could only ever guess wrong.
type OpenDBFunc func(path string) (*sql.DB, error)

// Caller is an AUTHENTICATED principal, resolved by the host before it calls
// the engine.
//
// Org is the validated tenant. The engine uses it VERBATIM and scopes every
// query by it, so it must be derived from a verified credential — never from a
// client-supplied header. A Caller with an empty Org is refused before any
// store access. This is the ONE way a tenant enters the engine; there is no
// second derivation path, and the engine cannot be handed a request to inspect.
type Caller struct {
	// Org is the validated tenant. Required.
	Org string
	// User is the caller's stable id, used as the key of their role grants.
	User string
	// IsAdmin marks a PLATFORM superuser (not an org admin): a manager in every
	// org. The host sets it only for a verified platform-level identity.
	IsAdmin bool
}

// Config configures an Engine.
type Config struct {
	// Dir is the directory holding the engine's database file. Created if absent.
	Dir string
	// File overrides the database file name within Dir (default "framework.db").
	File string
	// OpenDB opens the database. Nil means the plain hanzoai/sqlite driver.
	OpenDB OpenDBFunc
	// Logger is handed to lifecycle hooks as Event.Logger. Nil means a no-op logger.
	Logger luxlog.Logger
}

// Engine is a mounted DocType engine: its store, plus the logger it hands to
// hooks. It is an explicit value — construct it, hold it, close it. There is no
// package-global engine, so a host can run two (a test and a server, or two
// tenanted deployments) in one process without them colliding.
type Engine struct {
	store *Store
	log   luxlog.Logger
}

// DefaultFile is the database file name an Engine uses when Config.File is empty.
const DefaultFile = "framework.db"

// Open builds an Engine against the database in cfg.Dir.
func Open(cfg Config) (*Engine, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("framework.Open: empty Dir")
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("framework.Open: data dir: %w", err)
	}
	name := cfg.File
	if name == "" {
		name = DefaultFile
	}
	store, err := openStore(filepath.Join(cfg.Dir, name), cfg.OpenDB)
	if err != nil {
		return nil, fmt.Errorf("framework.Open: %w", err)
	}
	log := cfg.Logger
	if log == nil {
		log = luxlog.NewNoOpLogger()
	}
	return &Engine{store: store, log: log}, nil
}

// Close releases the engine's store. Idempotent.
func (e *Engine) Close() error {
	if e == nil || e.store == nil {
		return nil
	}
	err := e.store.Close()
	e.store = nil
	return err
}

// Store exposes the engine's data access for a first-party caller inside the
// trust boundary — notably a lifecycle hook reading or writing sibling
// documents, which must scope every call by Event.Org.
func (e *Engine) Store() *Store { return e.store }

// Log is the engine's logger.
func (e *Engine) Log() luxlog.Logger { return e.log }

// ready reports the engine is usable, so every entry point fails with one clear
// message instead of a nil dereference.
func (e *Engine) ready() error {
	if e == nil || e.store == nil {
		return fmt.Errorf("framework: engine not open")
	}
	return nil
}
