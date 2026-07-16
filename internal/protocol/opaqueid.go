package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewOpaqueID mints a new CouncilProtocolV1 identifier: a flat, opaque,
// gitmoot-core-minted token, deliberately distinct in shape from gitmoot's
// internal jobs.id (which is slash-delimited, e.g.
// "parent/delegation/<id>/retry/<n>", and is reused directly as a
// filesystem worktree path segment -- DESIGN.md decision log #2). The
// frozen schema's identifier pattern (^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$,
// council-protocol-v1.schema.json) excludes '/' and ':' for exactly this
// reason: a protocol id must never collide with, or be mistaken for, a real
// jobs.id. 16 crypto/rand bytes hex-encoded satisfies that pattern by
// construction and matches the opaque-id idiom already used elsewhere in
// this codebase (e.g. newCheckoutMutationOwnerToken in
// internal/workflow/checkout_lock.go, newSkillOptJudgeOutcomeID in
// internal/db/store.go) rather than introducing a ULID dependency this
// packet's single invariant does not need -- nothing here depends on
// lexicographic time-sortability, only on opaqueness and uniqueness.
//
// This package stays standalone (no schema/codegen/DB dependency) so both
// internal/db (minting jobs.protocol_task_id/protocol_attempt_id, W1-02a)
// and any later hand-written internal/protocol record struct can mint ids
// without pulling in anything beyond the standard library.
func NewOpaqueID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint opaque protocol id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
