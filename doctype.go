package framework

import "github.com/hanzoai/doctype"

// doctype.go re-exports the value layer.
//
// The schema vocabulary is DEFINED once, in github.com/hanzoai/doctype. These
// are Go type ALIASES and constant re-exports, not copies: framework.DocType
// and doctype.DocType are the same type, and a value crosses between them with
// no conversion. They exist so an app lane that builds on the engine needs ONE
// import to declare its content model, which is the common case:
//
//	framework.RegisterModule("cms", []framework.DocType{{
//	    Name: "Article",
//	    Fields: []framework.DocField{{Fieldname: "title", Fieldtype: framework.FieldData}},
//	}})
//
// A tool that only READS or VALIDATES schemas — a code generator, a UI
// renderer, a migration linter — should import doctype directly and link no
// database at all. That is the whole reason the value layer is a separate
// module.

// The schema types.
type (
	// DocType is a metadata definition — see doctype.DocType.
	DocType = doctype.DocType
	// DocField is one field in a DocType — see doctype.DocField.
	DocField = doctype.DocField
	// DocPerm is a role's rights on a DocType — see doctype.DocPerm.
	DocPerm = doctype.DocPerm
)

// The closed fieldtype set. A DocField with any other fieldtype is rejected at
// define time — an unknown type has no validation and is therefore a hole.
const (
	FieldData     = doctype.FieldData
	FieldInt      = doctype.FieldInt
	FieldFloat    = doctype.FieldFloat
	FieldCurrency = doctype.FieldCurrency
	FieldCheck    = doctype.FieldCheck
	FieldDate     = doctype.FieldDate
	FieldDatetime = doctype.FieldDatetime
	FieldText     = doctype.FieldText
	FieldSmall    = doctype.FieldSmall
	FieldLong     = doctype.FieldLong
	FieldRichText = doctype.FieldRichText
	FieldSelect   = doctype.FieldSelect
	FieldLink     = doctype.FieldLink
	FieldTable    = doctype.FieldTable
	FieldAttach   = doctype.FieldAttach
	FieldJSON     = doctype.FieldJSON
	FieldPassword = doctype.FieldPassword
)

// Roles and rights.
const (
	RoleSystemManager = doctype.RoleSystemManager
	RoleAll           = doctype.RoleAll

	RightRead   = doctype.RightRead
	RightWrite  = doctype.RightWrite
	RightCreate = doctype.RightCreate
	RightDelete = doctype.RightDelete
	RightSubmit = doctype.RightSubmit
	RightCancel = doctype.RightCancel
)

// The app-lane fixture registry. A lane declares its content model from a
// package init() and the engine installs it per-org; see doctype/module.go.
var (
	// RegisterModule declares the DocType fixtures a module installs.
	RegisterModule = doctype.RegisterModule
	// MarkAlwaysOn makes a module's fixtures resolve for every org with no
	// per-org install step.
	MarkAlwaysOn = doctype.MarkAlwaysOn
	// RegisteredModules is the sorted set of registered module names.
	RegisteredModules = doctype.RegisteredModules
	// AlwaysOnModules is the sorted set of modules marked always-on.
	AlwaysOnModules = doctype.AlwaysOnModules
)
