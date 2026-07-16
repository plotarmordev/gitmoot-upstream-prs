package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyV090Fixture copies the frozen real-v0.9.0 database (see
// testdata/fixtures/README.md for provenance) into a fresh temp file and
// returns its path. Store.Migrate mutates whatever database it opens -- it
// IS the upgrade path under test -- so every test using the fixture must get
// its own private copy rather than opening testdata/fixtures/gitmoot-v0.9.0.db
// directly, or a second test (or a second run of the same test) would upgrade
// an already-upgraded file instead of a real v0.9.0 predecessor.
func copyV090Fixture(t *testing.T) string {
	t.Helper()
	// The frozen fixture lives at the repo root (testdata/fixtures/), not
	// under internal/db/testdata -- `go test` runs with the package directory
	// as its working directory, hence the "../..".
	src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "gitmoot-v0.9.0.db"))
	if err != nil {
		t.Fatalf("read frozen v0.9.0 fixture: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "gitmoot.db")
	if err := os.WriteFile(dst, src, 0o600); err != nil {
		t.Fatalf("copy v0.9.0 fixture to temp file: %v", err)
	}
	return dst
}

// TestMigrationDigestDriftRejected pins R-05: an already-applied migration's
// content_digest, once recorded, must never silently stop matching its
// source. Fails on current base (schema_migrations has no migration_id/
// content_digest/predecessor_digest columns at all, so drift is structurally
// undetectable -- see testdata/fixtures/README.md and the packet's manual
// verification note for a direct before/after PRAGMA table_info comparison).
// Passes after: pass 1 backfills real digests for the real v0.9.0 fixture's
// historical migrations from the current (unmutated) migrations[] content;
// mutating one already-stamped historical migration's SQL body and reopening
// the SAME database in pass 2 must refuse to proceed, naming the exact
// version and both digests, instead of silently re-migrating past it.
func TestMigrationDigestDriftRejected(t *testing.T) {
	path := copyV090Fixture(t)

	// Pass 1: real upgrade, stamps digests for every historical migration
	// (including the fixture's own, pre-v0.9.0-unaware rows) from the
	// current, unmutated migrations[] content.
	store, err := Open(path)
	if err != nil {
		t.Fatalf("pass 1 Open (baseline stamp) returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close pass 1 store: %v", err)
	}

	// Mutate a migration that both the v0.9.0 fixture and pass 1 already
	// applied and stamped (well inside the fixture's own historical range --
	// see testdata/fixtures/README.md: schema_migrations tops out at version
	// 76 in that fixture), simulating exactly the R-05 scenario: someone
	// edited a historical migration's SQL body after it had already shipped.
	const tamperVersion = 10 // 1-based; migrations[tamperVersion-1]
	original := migrations[tamperVersion-1]
	migrations[tamperVersion-1] = original + "\n-- tampered by TestMigrationDigestDriftRejected\n"
	t.Cleanup(func() { migrations[tamperVersion-1] = original })

	// Pass 2: reopening the SAME database must refuse to proceed rather than
	// silently skip the drifted migration (applyMigration's ordinal
	// `exists > 0` guard would otherwise skip it with no error at all).
	store2, err := Open(path)
	if err == nil {
		_ = store2.Close()
		t.Fatalf("pass 2 Open succeeded after mutating an already-applied migration's content; want a digest mismatch error")
	}
	wantID := migrationID(tamperVersion)
	if got := err.Error(); !strings.Contains(got, wantID) || !strings.Contains(got, "digest mismatch") {
		t.Fatalf("pass 2 Open error = %q, want it to name migration %q and say \"digest mismatch\"", got, wantID)
	}
	t.Logf("got expected drift error: %v", err)
}

// TestMigrationUpgradeRealPredecessor is the positive-path proof: opening the
// real v0.9.0 fixture unmodified reaches the current head version, every
// historical row survives the upgrade intact, and every migration (the
// fixture's real historical ones plus every migration added since) ends up
// with a recorded digest that matches its current source and a predecessor
// chain that matches the migration immediately before it.
func TestMigrationUpgradeRealPredecessor(t *testing.T) {
	ctx := context.Background()
	path := copyV090Fixture(t)

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open real v0.9.0 fixture returned error: %v", err)
	}
	defer store.Close()

	// Reaches the current head version.
	var headVersion int
	if err := store.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&headVersion); err != nil {
		t.Fatalf("read max schema_migrations.version: %v", err)
	}
	if headVersion != len(migrations) {
		t.Fatalf("head version = %d, want %d (len(migrations))", headVersion, len(migrations))
	}

	// The fixture's non-empty jobs/job_events/tasks rows (see
	// testdata/fixtures/README.md: 2 jobs, 4 job_events, 1 task, produced
	// through the real v0.9.0 CLI) survive the upgrade intact.
	for table, want := range map[string]int{"jobs": 2, "job_events": 4, "tasks": 1} {
		var got int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if got != want {
			t.Fatalf("%s row count after upgrade = %d, want %d (fixture's historical rows must survive intact)", table, got, want)
		}
	}

	// Every migration's digest is recorded and matches its current source,
	// and the predecessor chain is unbroken.
	rows, err := store.db.QueryContext(ctx, `SELECT version, migration_id, content_digest, predecessor_digest FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations identity columns: %v", err)
	}
	defer rows.Close()

	var previousDigest string
	seen := 0
	for rows.Next() {
		var version int
		var gotID, gotDigest, gotPredecessor string
		if err := rows.Scan(&version, &gotID, &gotDigest, &gotPredecessor); err != nil {
			t.Fatalf("scan schema_migrations row: %v", err)
		}
		seen++
		if version < 1 || version > len(migrations) {
			t.Fatalf("unexpected schema_migrations.version %d outside migrations[] range 1..%d", version, len(migrations))
		}
		wantID := migrationID(version)
		wantDigest := sha256HexOf(migrations[version-1])
		if gotID != wantID {
			t.Fatalf("migration %d: migration_id = %q, want %q", version, gotID, wantID)
		}
		if gotDigest != wantDigest {
			t.Fatalf("migration %d (%s): content_digest = %q, want %q (digest of current source)", version, wantID, gotDigest, wantDigest)
		}
		if gotPredecessor != previousDigest {
			t.Fatalf("migration %d (%s): predecessor_digest = %q, want %q (preceding migration's content_digest)", version, wantID, gotPredecessor, previousDigest)
		}
		previousDigest = wantDigest
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema_migrations: %v", err)
	}
	if seen != len(migrations) {
		t.Fatalf("schema_migrations row count = %d, want %d (one per entry in migrations[])", seen, len(migrations))
	}
}
