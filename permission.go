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
// effective role set, and whether they are a manager (a platform superuser, an
// explicit System Manager, or the trust-on-first-use org owner).
type Access struct {
	Org     string
	User    string
	Roles   map[string]bool
	Manager bool
}

// Can reports whether the caller may perform `right` on a document of dt.
// A manager may do anything; otherwise the permission calculus decides.
//
// SECURE BY DEFAULT: there is no "empty perms means open" branch — see
// doctype.Grants. A role-less member of an org is denied.
func (a Access) Can(dt *doctype.DocType, right string) bool {
	if a.Manager {
		return true
	}
	return doctype.Grants(dt, a.Roles, right)
}

// resolve turns a Caller into an Access by reading the caller's per-org roles.
// It refuses a caller with no validated tenant — the engine will not guess an
// org, so a host that failed to authenticate gets an error, never a default.
func (e *Engine) resolve(_ context.Context, c Caller) (Access, error) {
	if c.Org == "" {
		return Access{}, fmt.Errorf("%w: valid principal required", ErrForbidden)
	}
	acc := Access{Org: c.Org, User: c.User, Roles: map[string]bool{doctype.RoleAll: true}}

	// A platform superuser is a manager everywhere.
	if c.IsAdmin {
		acc.Manager = true
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
func (e *Engine) accessDoc(ctx context.Context, c Caller, name, right string) (Access, doctype.DocType, error) {
	acc, err := e.resolve(ctx, c)
	if err != nil {
		return Access{}, doctype.DocType{}, err
	}
	dt, err := e.store.GetDocType(ctx, acc.Org, name)
	if err != nil {
		return Access{}, doctype.DocType{}, err
	}
	if !acc.Can(&dt, right) {
		return Access{}, doctype.DocType{}, fmt.Errorf("%w: permission denied: %s on %s", ErrForbidden, right, dt.Name)
	}
	return acc, dt, nil
}
