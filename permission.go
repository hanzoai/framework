package framework

import (
	"github.com/hanzoai/cloud/clients/principal"
	"github.com/zap-proto/zip"
)

// Permissions — per-org, DocType perms by role, enforced on every operation.
//
// The role source. IAM's JWT carries no per-user role set into the cloud binary
// (SanitizeIdentity restores only user/email/org/isAdmin), so the framework owns
// its role model per-org, exactly as Frappe records "Has Role" within a site: the
// fw_roles table maps (org, user) → roles, managed at /v1/framework/roles.
//
// The gate. Every handler resolves an `access` through ONE seam (resolveAccess),
// which begins with the ONE tenant derivation (principal.Tenant) and never a
// second path. From the validated principal it derives the caller's effective
// roles and whether they are a manager, then answers can(doctype, right).
const (
	// RoleSystemManager is the admin role: it manages DocTypes + role assignments
	// and is granted every document right. Mirrors Frappe's System Manager.
	RoleSystemManager = "System Manager"
	// RoleAll is the implicit role every validated org member holds (Frappe "All").
	RoleAll = "All"
)

// rights.
const (
	rightRead   = "read"
	rightWrite  = "write"
	rightCreate = "create"
	rightDelete = "delete"
	rightSubmit = "submit"
	rightCancel = "cancel"
)

// access is a resolved caller: their org (the validated tenant), effective role
// set, user id, and whether they are a manager (global admin, an explicit System
// Manager, or a member of an as-yet-unconfigured org — the bootstrap).
type access struct {
	org     string
	user    string
	roles   map[string]bool
	manager bool
}

// resolveAccess is the ONE authorization seam. It returns (access, nil) for a
// validated principal of a real org, or a 403 error otherwise — there is no
// second org-derivation path in this package. It reads the caller's per-org roles
// and computes manager status (global admin OR System Manager OR bootstrap).
func (s *svc) resolveAccess(c *zip.Ctx) (access, error) {
	org, ok := principal.Tenant(c)
	if !ok {
		return access{}, zip.ErrForbidden("valid principal required")
	}
	acc := access{org: org, user: c.User(), roles: map[string]bool{RoleAll: true}}

	// Global admin (validated owner == AdminOrg) is a manager everywhere.
	if c.IsAdmin() {
		acc.manager = true
		acc.roles[RoleSystemManager] = true
		return acc, nil
	}

	assigned, err := s.store.RolesFor(c.Context(), org, acc.user)
	if err != nil {
		return access{}, zip.Errorf(500, "resolve roles: %v", err)
	}
	for _, r := range assigned {
		acc.roles[r] = true
		if r == RoleSystemManager {
			acc.manager = true
		}
	}
	// No implicit manager here: a member's authority is EXACTLY their assigned
	// roles (plus global admin above). The one-time owner seeding lives in
	// managerOnly, so a role-less member is never silently a manager on the read /
	// document-CRUD path — permless doctypes are default-closed (see can).
	return acc, nil
}

// can reports whether the caller may perform `right` on a document of dt.
//
//   - A manager (System Manager / global admin) may do anything.
//   - Otherwise, at least one of the caller's roles must carry `right` in the
//     DocType's permission rows.
//
// SECURE BY DEFAULT: there is NO "empty perms means open to all" branch. A
// DocType with no role grants is manager-only, never open to every org member —
// the DENY default. (DocType define-time seeds a System Manager grant, so a
// stored doctype is never silently permless; this loop is the enforcement, and it
// fails closed for a role-less member regardless.)
func (a access) can(dt *DocType, right string) bool {
	if a.manager {
		return true
	}
	for _, p := range dt.Perms {
		if !a.roles[p.Role] {
			continue
		}
		if permGrants(p, right) {
			return true
		}
	}
	return false
}

func permGrants(p DocPerm, right string) bool {
	switch right {
	case rightRead:
		return p.Read
	case rightWrite:
		return p.Write
	case rightCreate:
		return p.Create
	case rightDelete:
		return p.Delete
	case rightSubmit:
		return p.Submit
	case rightCancel:
		return p.Cancel
	default:
		return false
	}
}

// managerOnly is the meta-permission gate for managing DocType definitions and
// role assignments: only a manager may proceed.
//
// OWNER SEEDING (trust-on-first-use). The FIRST validated principal to administer
// an org that has NO role assignments becomes its System Manager — persisted, ONCE
// — i.e. the org owner/creator. Every later member has NO privilege until the
// owner grants a role. This is strictly narrower than the previous "any member is
// a manager until a role exists" and can NEVER cross a tenant (org is the
// validated tenant, principal.Tenant). It resolves the setup chicken-and-egg (an
// org with no roles could otherwise never define its first DocType) without a
// footgun: exactly one member is auto-granted, deterministically the first setter.
func (s *svc) managerOnly(c *zip.Ctx) (access, error) {
	acc, err := s.resolveAccess(c)
	if err != nil {
		return access{}, err
	}
	if acc.manager {
		return acc, nil
	}
	has, err := s.store.OrgHasRoles(c.Context(), acc.org)
	if err != nil {
		return access{}, zip.Errorf(500, "role bootstrap: %v", err)
	}
	if !has {
		if err := s.store.AssignRole(c.Context(), acc.org, acc.user, RoleSystemManager); err != nil {
			return access{}, zip.Errorf(500, "seed owner: %v", err)
		}
		acc.roles[RoleSystemManager] = true
		acc.manager = true
		return acc, nil
	}
	return access{}, zip.ErrForbidden("System Manager role required")
}
