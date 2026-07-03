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

	// Bootstrap: an org with NO role assignments is unconfigured — a validated
	// member acts as System Manager for their OWN org (never another's, since org
	// is the validated tenant) so they can define doctypes, then lock down by
	// assigning roles. Once the first role is assigned, strict enforcement applies.
	if !acc.manager {
		has, err := s.store.OrgHasRoles(c.Context(), org)
		if err != nil {
			return access{}, zip.Errorf(500, "role bootstrap: %v", err)
		}
		if !has {
			acc.manager = true
			acc.roles[RoleSystemManager] = true
		}
	}
	return acc, nil
}

// can reports whether the caller may perform `right` on a document of dt.
//
//   - A manager (System Manager / global admin / bootstrap) may do anything.
//   - A DocType with NO permissions is open to any validated org member (parity
//     with the existing per-org subsystems, e.g. crm — never weaker).
//   - Otherwise, at least one of the caller's roles must carry `right` in the
//     DocType's permission rows.
func (a access) can(dt *DocType, right string) bool {
	if a.manager {
		return true
	}
	if len(dt.Perms) == 0 {
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
func (s *svc) managerOnly(c *zip.Ctx) (access, error) {
	acc, err := s.resolveAccess(c)
	if err != nil {
		return access{}, err
	}
	if !acc.manager {
		return access{}, zip.ErrForbidden("System Manager role required")
	}
	return acc, nil
}
