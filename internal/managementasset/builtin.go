package managementasset

import _ "embed"

//go:embed builtin/management.html
var builtinManagementHTML []byte

// BuiltinManagementHTML returns the bundled management shell page.
func BuiltinManagementHTML() []byte {
	return append([]byte(nil), builtinManagementHTML...)
}
