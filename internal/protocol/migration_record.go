// Package protocol holds hand-written Go bindings for CouncilProtocolV1
// record kinds (docs/protocol/council-protocol-v1/council-protocol-v1.schema.json,
// frozen). Every kind gets its own hand-written struct, added by whichever
// packet first persists that kind -- there is no codegen pipeline for this
// package and none is planned (Council correction packet Review #5 / operator
// ruling R1).
//
// MigrationRecord below is this package's first consumer, added by W1-01. It
// is PROVISIONAL: W1-02b adds the golden-fixture conformance harness that
// validates hand-written structs against the frozen schema, so until W1-02b
// lands this struct is correct by inspection against the schema's
// $defs.MigrationRecord, not yet machine-checked.
package protocol

// ProtocolVersion is the frozen CouncilProtocolV1 version string (schema
// $defs.ProtocolVersion, a const). Any field change that is not purely
// additive ships as a new major protocol version instead of mutating this
// constant.
const ProtocolVersion = "1.0.0"

// ContentDigest is a sha256 content digest (schema $defs.ContentDigest).
// Locked to sha256 only for v1 -- no multi-algorithm branch. Hex is always
// the lowercase 64-character sha256 hex digest of the exact bytes addressed,
// computed by gitmoot-core itself, never accepted verbatim from a caller.
type ContentDigest struct {
	Algo string `json:"algo"` // always "sha256"
	Hex  string `json:"hex"`
}

// NewSHA256Digest wraps an already-computed lowercase sha256 hex digest as a
// ContentDigest. It does not hash anything itself: the protocol requires the
// digest to be computed by the core from bytes it read itself, never an
// unkeyed hash accepted from outside (coding rule 6), so the hashing always
// happens at the call site closest to the source bytes.
func NewSHA256Digest(hex string) ContentDigest {
	return ContentDigest{Algo: "sha256", Hex: hex}
}

// MigrationRecord is the hand-written binding for schema
// $defs.MigrationRecord. It forecloses R-05 (an ordinal-only
// schema_migrations ledger cannot detect content drift on an already-applied
// migration): MigrationID is a stable identity independent of both Sequence
// and ContentDigest, ContentDigest pins the exact SQL body a migration was
// applied with, and PredecessorDigest chains each record to the one before
// it so a migration spliced into the middle of history is also detectable
// (R-06). PredecessorDigest is a pointer because the schema allows null for
// the first migration in the chain, which has no predecessor.
type MigrationRecord struct {
	ProtocolVersion   string         `json:"protocol_version"`
	RecordID          string         `json:"record_id"`
	RecordType        string         `json:"record_type"` // const "migration_record"
	IssuedBy          string         `json:"issued_by"`   // const "gitmoot-core"
	IssuedAt          string         `json:"issued_at"`   // RFC3339 UTC, core-stamped
	MigrationID       string         `json:"migration_id"`
	Sequence          int            `json:"sequence"`
	ContentDigest     ContentDigest  `json:"content_digest"`
	PredecessorDigest *ContentDigest `json:"predecessor_digest"`
	AppliedAt         string         `json:"applied_at"` // RFC3339 UTC
}
