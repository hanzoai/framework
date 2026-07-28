package framework

import (
	"context"
	"fmt"

	"github.com/hanzoai/doctype"
)

// Permissions — per-org, DocType perms by role, enforced on every operation.
//
// The role source. An identity provider's token carries no per-user role set
// into this engine, so the framework owns its role model per-org, exactly as
// Frappe records "Has Role" within a site: the fw_roles table maps (org, user)
// → roles.
//
// The split. Deciding whether a right HOLDS is a pure function of values and
// lives in doctype.Grants. RESOLVING who the caller is — reading their roles,
// honouring a platform superuser, seeding the org owner on first use — reads
// tenant state and lives here. The host resolves the Caller (from a request, a
// CLI session, a job) and hands it in; the engine never derives a tenant itself.
// That is the ONE authorization seam and there is no second path.

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
func (e *Engine) resolve(ctx context.Context, c Caller) (Access, error) {
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

	assigned, err := e.store.RolesFor(ctx, acc.Org, acc.User)
	if err != nil {
		return Access{}, fmt.Errorf("resolve roles: %w", err)
	}
	for _, r := range assigned {
		acc.Roles[r] = true
		if r == doctype.RoleSystemManager {
			acc.Manager = true
		}
	}
	// No implicit manager here: a member's authority is EXACTLY their assigned
	// roles (plus the superuser above). The one-time owner seeding lives in
	// resolveManager, so a role-less member is never silently a manager on the
	// read / document-CRUD path — permission-less doctypes are default-closed.
	return acc, nil
}

// resolveManager is the meta-permission gate for managing DocType definitions,
// role assignments and module installs: only a manager may proceed.
//
// OWNER SEEDING (trust-on-first-use, ATOMIC). The FIRST validated principal to
// administer an org that has NO role assignments becomes its System Manager —
// persisted, ONCE — i.e. the org owner/creator. Every later member has NO
// privilege until the owner grants a role. It can NEVER cross a tenant (the org
// is the caller's validated tenant) and it resolves the setup chicken-and-egg:
// an org with no roles could otherwise never define its first DocType.
//
// The seed is a SINGLE conditional INSERT (store.SeedOwnerIfUnowned), so
// "exactly one" holds under concurrency: a check-then-insert let several
// simultaneous first-callers each seed themselves (Red measured 3–6). If our
// insert did not win we re-resolve — another request may have granted this user
// a role in the race — and refuse only if still non-manager.
func (e *Engine) resolveManager(ctx context.Context, c Caller) (Access, error) {
	acc, err := e.resolve(ctx, c)
	if err != nil {
		return Access{}, err
	}
	if acc.Manager {
		return acc, nil
	}
	seeded, err := e.store.SeedOwnerIfUnowned(ctx, acc.Org, acc.User)
	if err != nil {
		return Access{}, fmt.Errorf("seed owner: %w", err)
	}
	if seeded {
		acc.Roles[doctype.RoleSystemManager] = true
		acc.Manager = true
		return acc, nil
	}
	// Our seed did not win (the org is now owned). Re-resolve: a concurrent
	// grant may have made this caller a manager; otherwise refuse.
	assigned, err := e.store.RolesFor(ctx, acc.Org, acc.User)
	if err != nil {
		return Access{}, fmt.Errorf("resolve roles: %w", err)
	}
	for _, r := range assigned {
		if r == doctype.RoleSystemManager {
			acc.Roles[r] = true
			acc.Manager = true
			return acc, nil
		}
	}
	return Access{}, fmt.Errorf("%w: System Manager role required", ErrForbidden)
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
