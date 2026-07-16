package db

import (
	"context"
	"fmt"
)

// migrationID mints a stable identifier for the migration at 1-based ordinal
// `version` (the same numbering applyMigration already uses for
// schema_migrations.version). It is deliberately derived from POSITION, not
// from the migration's SQL body: content_digest already captures content, and
// R-05's drift detection depends on the SAME migration_id surfacing a
// DIFFERENT digest when its source is edited after the fact -- an id derived
// from content would instead just mint a brand new id on every edit, silently
// reopening the exact hole this packet closes. None of gitmoot's historical
// migrations were named at the time they were appended, so "mig-%04d" is the
// leanest stable scheme available; it is intentionally a distinct TEXT value
// (not the bare integer) from the schema_migrations.version column it is
// derived from, matching the protocol schema's requirement that migration_id
// be more than merely the ordinal.
func migrationID(version int) string {
	return fmt.Sprintf("mig-%04d", version)
}

// migrationIdentityRow mirrors one schema_migrations row's identity columns.
type migrationIdentityRow struct {
	version           int
	migrationID       string
	contentDigest     string
	predecessorDigest string
}

// reconcileMigrationIdentity is the identity half of Migrate. For every row in
// schema_migrations (one per already-applied migration, version 1..N):
//
//   - if the row predates this packet (migration_id = ”, the ALTER TABLE's
//     DEFAULT), it is backfilled: migration_id, content_digest, and
//     predecessor_digest are computed from the CURRENT migrations[] slice and
//     stamped in place. One-time per row, idempotent, matches the
//     backfillJobRootID pattern (store.go) exactly.
//   - if the row is already stamped, its stored content_digest and
//     predecessor_digest are re-checked against a fresh digest of the CURRENT
//     source on every boot (not just the first). A mismatch means the
//     migration's SQL body -- or the body of the migration immediately before
//     it -- was edited after being applied to this database, and Migrate
//     refuses to proceed rather than silently serving a schema that no longer
//     matches the code that produced it (R-05). The error names the exact
//     migration_id, version, and both digests so the operator can tell which
//     historical migration drifted without re-deriving it by hand.
//
// A row whose version has no corresponding entry in the current migrations
// slice (a database written by a newer build than the one reading it) is left
// untouched rather than guessed at; reconciliation only ever looks backward
// from the current head.
func (s *Store) reconcileMigrationIdentity(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT version, migration_id, content_digest, predecessor_digest FROM schema_migrations ORDER BY version`)
	if err != nil {
		return err
	}
	var existing []migrationIdentityRow
	for rows.Next() {
		var r migrationIdentityRow
		if err := rows.Scan(&r.version, &r.migrationID, &r.contentDigest, &r.predecessorDigest); err != nil {
			rows.Close()
			return err
		}
		existing = append(existing, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var previousDigest string
	for _, r := range existing {
		if r.version < 1 || r.version > len(migrations) {
			// Out of range for this build's migrations[]; nothing to reconcile
			// against, so preserve whatever chain value it already carries.
			previousDigest = r.contentDigest
			continue
		}
		wantID := migrationID(r.version)
		wantDigest := sha256HexOf(migrations[r.version-1])
		wantPredecessor := previousDigest

		switch {
		case r.migrationID == "":
			if _, err := tx.ExecContext(ctx, `UPDATE schema_migrations SET migration_id = ?, content_digest = ?, predecessor_digest = ? WHERE version = ?`,
				wantID, wantDigest, wantPredecessor, r.version); err != nil {
				return err
			}
		case r.contentDigest != wantDigest:
			return fmt.Errorf("migration %d (%s): content digest mismatch — recorded digest %s does not match the digest of the migration's current source (%s); historical migration content must never change after it has been applied (R-05)",
				r.version, wantID, r.contentDigest, wantDigest)
		case r.predecessorDigest != wantPredecessor:
			return fmt.Errorf("migration %d (%s): predecessor digest mismatch — recorded predecessor digest %s does not match the preceding migration's current digest (%s); the migration chain has been reordered or spliced (R-05/R-06)",
				r.version, wantID, r.predecessorDigest, wantPredecessor)
		}
		previousDigest = wantDigest
	}
	return tx.Commit()
}
