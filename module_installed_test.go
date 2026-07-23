package framework

import (
	"context"
	"testing"
)

// TestModuleInstalled_OrgScoped proves the module-granularity observe read
// (bound as guide's ModuleInstalled growth seam) is strictly org-scoped and
// honest-degrading: it reports a module present ONLY for the org that installed it,
// an unknown module reads false, and the boolean never exposes another org's data.
func TestModuleInstalled_OrgScoped(t *testing.T) {
	s, done := mountForIngest(t) // sets the package-global `mounted` ModuleInstalled reads
	defer done()
	ctx := context.Background()

	resetModules()
	t.Cleanup(resetModules)
	RegisterModule("shop", []DocType{{Name: "Widget", Fields: []DocField{{Fieldname: "sku", Fieldtype: FieldData}}}})

	// orgA installs the module's doctype; orgB installs nothing.
	if _, err := s.State.store.CreateDocType(ctx, "orgA", DocType{
		Name: "Widget", Module: "shop",
		Fields: []DocField{{Fieldname: "sku", Fieldtype: FieldData}},
		Perms:  []DocPerm{{Role: RoleSystemManager, Read: true, Write: true, Create: true, Delete: true}},
	}); err != nil {
		t.Fatalf("install: %v", err)
	}

	if !ModuleInstalled(ctx, "orgA", "shop") {
		t.Fatal("orgA installed shop → ModuleInstalled must be true")
	}
	if ModuleInstalled(ctx, "orgB", "shop") {
		t.Fatal("cross-tenant leak: orgB must NOT observe orgA's installed module")
	}
	if ModuleInstalled(ctx, "orgA", "not-a-module") {
		t.Fatal("an unregistered module must read false")
	}
}
