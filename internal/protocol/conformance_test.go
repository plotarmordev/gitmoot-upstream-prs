package protocol

// Golden-fixture conformance harness (W1-02b). Every hand-written struct in
// this package is registered (see registry.go, RegisterFixture) alongside a
// golden JSON fixture under fixtures/; this file is the only place that
// walks the registry and checks each struct's marshaled JSON against the
// frozen schema. Kept test-only on purpose: github.com/santhosh-tekuri/
// jsonschema/v6 is the standard, actively maintained Go JSON-Schema
// implementation (draft 2020-12 support, matches this schema's $schema), but
// the protocol package's structs must stay importable from production
// binaries without dragging a validator library along -- Go only links a
// package's non-_test.go imports into a production build, so confining the
// import to this file is sufficient (operator ruling R1).
//
// v6, not v5: the frozen schema's $defs.BranchName uses lookahead/lookbehind
// (`(?!/)`, `(?<!/)`) that Go's standard regexp package (RE2) cannot compile.
// Both v5 and v6 meta-validate an ENTIRE added resource document against the
// draft meta-schema before any $def can be compiled out of it (there is no
// way to compile just one $def without that whole-document pass), and that
// meta-validation always checks every "pattern" string's format -- so even
// though this harness only ever compiles $defs.MigrationRecord, BranchName's
// pattern living elsewhere in the same document still has to parse. v6 adds
// Compiler.UseRegexpEngine specifically for this: swapping in
// github.com/dlclark/regexp2 (ECMAScript mode, which does support
// lookaround) lets the real, unmodified, frozen schema file compile as-is.
import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dlclark/regexp2"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaDefURL builds the compiler URL for one $defs entry of the vendored,
// read-only frozen schema copy (testdata/council-protocol-v1.schema.json --
// see that file's header in the source repo for the freeze pointer). Doing
// this per-def (rather than compiling the whole document once and picking a
// def out of it) is what lets AddResource+Compile validate a bare
// MigrationRecord instance directly, instead of every fixture needing to be
// wrapped in the top-level oneOf envelope.
func schemaDefURL(defName string) string {
	return "council-protocol-v1.schema.json#/$defs/" + defName
}

// dlclarkRegexp adapts *regexp2.Regexp to jsonschema.Regexp (MatchString +
// String), the shape Compiler.UseRegexpEngine requires.
type dlclarkRegexp regexp2.Regexp

func (re *dlclarkRegexp) MatchString(s string) bool {
	matched, err := (*regexp2.Regexp)(re).MatchString(s)
	return err == nil && matched
}

func (re *dlclarkRegexp) String() string {
	return (*regexp2.Regexp)(re).String()
}

// dlclarkCompile is the jsonschema.RegexpEngine plugged in below -- see this
// file's header comment for why the frozen schema needs an ECMAScript-mode
// (lookaround-capable) engine instead of Go's stdlib RE2 regexp.
func dlclarkCompile(s string) (jsonschema.Regexp, error) {
	re, err := regexp2.Compile(s, regexp2.ECMAScript)
	if err != nil {
		return nil, err
	}
	return (*dlclarkRegexp)(re), nil
}

func newCompiler(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "council-protocol-v1.schema.json"))
	if err != nil {
		t.Fatalf("open vendored frozen schema: %v", err)
	}
	defer f.Close()

	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatalf("unmarshal vendored frozen schema: %v", err)
	}

	c := jsonschema.NewCompiler()
	c.UseRegexpEngine(dlclarkCompile)
	if err := c.AddResource("council-protocol-v1.schema.json", doc); err != nil {
		t.Fatalf("load vendored frozen schema: %v", err)
	}
	return c
}

// canonicalize round-trips arbitrary JSON bytes through Go's generic decode
// so two byte-different-but-semantically-equal documents (key order,
// insignificant whitespace) compare equal with reflect.DeepEqual.
func canonicalize(t *testing.T, label string, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal %s as generic JSON: %v", label, err)
	}
	return v
}

// TestGoStructsValidateAgainstFrozenSchema is W1-02b's regression test. It
// fails on current base because internal/protocol did not exist at all
// before W1-01, and no conformance harness existed before this packet -- no
// registered struct's JSON was ever machine-checked against the frozen
// schema. It passes after: every kind in the registry (currently just
// MigrationRecord, per R1's narrowed scope -- later packets add their own
// registrants, reusing this same test unmodified) round-trips its golden
// fixture byte-for-byte after canonicalization and validates cleanly against
// its matching $defs entry.
func TestGoStructsValidateAgainstFrozenSchema(t *testing.T) {
	bindings := RegisteredFixtures()
	if len(bindings) == 0 {
		t.Fatal("fixtureRegistry is empty -- expected at least MigrationRecord to be registered by migration_record.go's init()")
	}

	compiler := newCompiler(t)

	for _, b := range bindings {
		b := b
		t.Run(b.RecordType, func(t *testing.T) {
			fixturePath := filepath.Join("fixtures", b.FixtureFile)
			golden, err := os.ReadFile(fixturePath)
			if err != nil {
				t.Fatalf("read golden fixture %s: %v", fixturePath, err)
			}

			// Unmarshal the golden fixture into a fresh instance of the
			// bound struct, then marshal it straight back out -- this is
			// the "populated instance" the struct produces, exercised
			// through its own json tags rather than the fixture's raw
			// bytes.
			inst := b.New()
			if err := json.Unmarshal(golden, inst); err != nil {
				t.Fatalf("unmarshal golden fixture into %T: %v", inst, err)
			}
			remarshaled, err := json.Marshal(inst)
			if err != nil {
				t.Fatalf("marshal %T back to JSON: %v", inst, err)
			}

			// Round-trip check: the struct's marshaled JSON must be
			// byte-diff-free from the golden fixture after
			// canonicalization -- proves the struct's json tags capture
			// every field the fixture has, with no silent drops or
			// additions.
			wantCanon := canonicalize(t, "golden fixture", golden)
			gotCanon := canonicalize(t, "re-marshaled struct", remarshaled)
			if !reflect.DeepEqual(wantCanon, gotCanon) {
				t.Fatalf("%s round-trip mismatch:\n golden:       %s\n re-marshaled: %s", b.RecordType, golden, remarshaled)
			}

			// Schema validation: the re-marshaled struct output (not just
			// the fixture file) must validate against the schema's
			// matching $defs entry.
			schema, err := compiler.Compile(schemaDefURL(b.SchemaDef))
			if err != nil {
				t.Fatalf("compile schema $defs/%s: %v", b.SchemaDef, err)
			}
			var instance any
			if err := json.Unmarshal(remarshaled, &instance); err != nil {
				t.Fatalf("unmarshal re-marshaled struct for validation: %v", err)
			}
			if err := schema.Validate(instance); err != nil {
				t.Fatalf("%s (fixture %s) failed schema validation against $defs/%s: %v", b.RecordType, b.FixtureFile, b.SchemaDef, err)
			}
		})
	}
}

// TestMigrationRecordFixtureMatchesSchema is W1-02b's positive-path proof.
// Unlike the generic loop above, this pins the specific migration_record
// fixture's provenance and content: fixtures/migration_record.json was
// captured from a real Store.Migrate run against a fresh database (the
// migration_id, content_digest, and applied_at values are
// internal/db.reconcileMigrationIdentity's actual output for migration 1,
// read back via the schema_migrations table -- not hand-invented hex), so a
// pass here proves the harness catches drift against a real migration
// identity record, not just a hand-tuned unit fixture built to satisfy the
// schema by construction.
func TestMigrationRecordFixtureMatchesSchema(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("fixtures", "migration_record.json"))
	if err != nil {
		t.Fatalf("read migration_record.json: %v", err)
	}

	var rec MigrationRecord
	if err := json.Unmarshal(golden, &rec); err != nil {
		t.Fatalf("unmarshal migration_record.json: %v", err)
	}

	// The specific real values this fixture pins (see the file's header
	// comment / this test's doc comment for provenance): migration 1 is the
	// first migration ever applied, so it has sequence 0 and a nil
	// predecessor_digest -- the schema's "oneOf ContentDigest|null" branch
	// for predecessor_digest is only exercised by a fixture like this one.
	if rec.MigrationID != "mig-0001" {
		t.Fatalf("migration_id = %q, want %q (real output of migrationID(1))", rec.MigrationID, "mig-0001")
	}
	if rec.Sequence != 0 {
		t.Fatalf("sequence = %d, want 0 (first migration in the chain)", rec.Sequence)
	}
	if rec.PredecessorDigest != nil {
		t.Fatalf("predecessor_digest = %+v, want nil (first migration has no predecessor)", rec.PredecessorDigest)
	}
	if rec.ContentDigest.Algo != "sha256" || len(rec.ContentDigest.Hex) != 64 {
		t.Fatalf("content_digest = %+v, want a 64-hex-char sha256 digest", rec.ContentDigest)
	}

	compiler := newCompiler(t)
	schema, err := compiler.Compile(schemaDefURL("MigrationRecord"))
	if err != nil {
		t.Fatalf("compile schema $defs/MigrationRecord: %v", err)
	}
	var instance any
	if err := json.Unmarshal(golden, &instance); err != nil {
		t.Fatalf("unmarshal fixture for validation: %v", err)
	}
	if err := schema.Validate(instance); err != nil {
		t.Fatalf("migration_record.json failed schema validation: %v", err)
	}
}
