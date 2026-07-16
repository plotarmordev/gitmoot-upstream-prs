package protocol

// FixtureBinding pairs one hand-written struct with the schema $def and
// golden fixture file the conformance harness (internal/protocol/
// conformance_test.go, W1-02b) checks it against. Each packet that adds a
// new record kind to this package registers exactly one binding for it,
// alongside the struct definition, via RegisterFixture in an init() -- this
// is the "registered with W1-02b's harness" step later packets' plans refer
// to. The harness itself lives in a _test.go file (see conformance_test.go
// for why), but the registry is production code so every kind's binding is
// declared once, next to the struct it describes, instead of the test file
// needing to know the full list of kinds in advance.
type FixtureBinding struct {
	// RecordType is the schema's record_type discriminant for this kind
	// (e.g. "migration_record"), matching the struct's RecordType field.
	RecordType string
	// SchemaDef is the matching definition name under the frozen schema's
	// $defs (e.g. "MigrationRecord").
	SchemaDef string
	// FixtureFile is the golden instance's path, relative to
	// internal/protocol/fixtures/.
	FixtureFile string
	// New returns a pointer to a zero-value instance of the bound struct,
	// used by the harness to unmarshal the golden fixture before
	// re-marshaling it for schema validation and round-trip comparison.
	New func() any
}

// fixtureRegistry accumulates one FixtureBinding per record kind. Appended to
// only by each kind's own init(), never read or mutated outside this
// package's tests.
var fixtureRegistry []FixtureBinding

// RegisterFixture adds a kind's fixture binding to the registry. Called from
// an init() next to the kind's struct definition (see MigrationRecord in
// migration_record.go for the first and, so far, only registrant).
func RegisterFixture(b FixtureBinding) {
	fixtureRegistry = append(fixtureRegistry, b)
}

// RegisteredFixtures returns the accumulated bindings. Exported (rather than
// giving conformance_test.go direct access to the unexported slice) so the
// registry stays read-only from outside the package, matching how
// fixtureRegistry itself is never mutated except via RegisterFixture.
func RegisteredFixtures() []FixtureBinding {
	return fixtureRegistry
}
