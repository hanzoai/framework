package framework

import (
	"context"
	"fmt"

	"github.com/hanzoai/doctype"
)

// Permissions — per-org, DocType perms by role, enforced on every operation.
//
// The role source is Hanzo IAM, carried on the Caller. The engine holds no
// identity, no membership and no grants: the host resolves them and hands them
// in, exactly as it hands in the tenant.
//
// The split. Deciding whether a right HOLDS is a pure function of values and
// lives in doctype.Grants. RESOLVING who the caller is happens before the engine
// is called at all. That is the ONE authorization seam and there is no second
// path.

// Access is a resolved caller: the validated tenant, their identity, their
// effective role set, and the two scopes above the roles.
//
// The two are not one. Manager is org-scoped — a caller IAM gave System Manager
// in THIS org — and Super is the platform's own cross-tenant identity. Holding
// them as one value made an org's own administrator into platform sudo, which is
// the escalation the scopes exist to keep apart.
type Access struct {
	Org   string
	User  string
	Roles map[string]bool
	// Manager may define DocTypes and install modules in this org. That is the
	// META-permission — what the schema itself is — and it is not a right on any
	// document.
	Manager bool
	// Super is the platform's cross-tenant identity, which overrides a DocType's
	// permission table. An org's own System Manager does not: a DocType that
	// withholds a right from the role withholds it.
	Super bool
}

// Can reports whether the caller may perform `right` on a document of dt.
//
// Three questions in order. Does the DocType admit the right at all — an
// immutable record has no writer and no remover, and platform sudo is not an
// exception to what a record IS. Is the caller platform sudo, which overrides a
// permission table. Otherwise the calculus decides.
//
// Manager is absent from that list on purpose. It used to answer true ahead of
// the table, which left a DocType unable to withhold anything from its own org's
// administrator: a row naming System Manager with Read alone granted nothing and
// withheld nothing, so an append-only trail had a writer after all. A caller
// holding System Manager still carries the role, so every DocType that names it
// resolves through the same table as every other role.
//
// SECURE BY DEFAULT: there is no "empty perms means open" branch — see
// doctype.Grants. A role-less member of an org is denied.
func (a Access) Can(dt *doctype.DocType, right string) bool {
	if !doctype.Admits(dt, right) {
		return false
	}
	return a.Super || doctype.Grants(dt, a.Roles, right)
}

// resolve turns a Caller into an Access by reading the caller's per-org roles.
// It refuses a caller with no validated tenant — the engine will not guess an
// org, so a host that failed to authenticate gets an error, never a default.
func (e *Engine) resolve(_ context.Context, c Caller) (Access, error) {
	if c.Org == "" {
		return Access{}, fmt.Errorf("%w: valid principal required", ErrForbidden)
	}
	acc := Access{Org: c.Org, User: c.User, Roles: map[string]bool{doctype.RoleAll: true}}

	// A platform superuser is both scopes everywhere.
	if c.IsAdmin {
		acc.Manager, acc.Super = true, true
		acc.Roles[doctype.RoleSystemManager] = true
		return acc, nil
	}
	for _, r := range c.Roles {
		acc.Roles[r] = true
		if r == doctype.RoleSystemManager {
			acc.Manager = true
		}
	}
	return acc, nil
}

// resolveManager is the meta-permission gate for managing DocType definitions and
// module installs: only a manager may proceed.
//
// A manager is a platform superuser or a caller IAM gives the System Manager
// role. There is no trust-on-first-use seeding: an org's first administrator is
// granted in IAM, which is where every other grant is made.
func (e *Engine) resolveManager(ctx context.Context, c Caller) (Access, error) {
	acc, err := e.resolve(ctx, c)
	if err != nil {
		return Access{}, err
	}
	if !acc.Manager {
		return Access{}, fmt.Errorf("%w: System Manager role required", ErrForbidden)
	}
	return acc, nil
}

// accessDoc resolves the caller AND loads the target DocType, enforcing `right`
// in one place. It is the ONE gate every document operation passes through.
func (e *Engine) accessDoc(ctx context.Context, c Caller, id doctype.ID, right string) (Access, doctype.DocType, error) {
	acc, err := e.resolve(ctx, c)
	if err != nil {
		return Access{}, doctype.DocType{}, err
	}
	dt, err := e.store.GetDocType(ctx, acc.Org, id)
	if err != nil {
		return Access{}, doctype.DocType{}, err
	}
	if !acc.Can(&dt, right) {
		return Access{}, doctype.DocType{}, fmt.Errorf("%w: permission denied: %s on %s", ErrForbidden, right, dt.ID())
	}
	return acc, dt, nil
}
