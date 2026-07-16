#!/usr/bin/env bash
# gen-v090-fixture.sh regenerates testdata/fixtures/gitmoot-v0.9.0.db.
#
# This is a ONE-TIME, implementer-run generation step (Council correction
# packet W1-01 / R-06), not part of the test suite's runtime path. It exists
# so the frozen fixture's provenance is reproducible and auditable, not so it
# runs on every `go test`. Re-run it only if the fixture needs to be replaced
# (e.g. a future packet needs a fixture pinned to a different historical
# release).
#
# What it does, in order:
#   1. Downloads the real v0.9.0 release binary for this host's platform from
#      jerryfane/gitmoot's GitHub releases (never hand-built — R-06 requires a
#      REAL historical binary exercising its own real code paths, not a
#      schema reconstructed from reading the migrations slice).
#   2. Runs that binary, unmodified, against a throwaway --home directory
#      (never the operator's default ~/.gitmoot) through a real init -> repo
#      add -> goal import -> agent start -> job open/close/record cycle, so
#      the resulting database has genuine non-empty rows in jobs, job_events,
#      and tasks -- produced by v0.9.0's own CLI, not hand-inserted SQL.
#   3. Checkpoints the WAL (wal_checkpoint(TRUNCATE)) and VACUUMs so no WAL/
#      SHM sidecar files or dead pages ship in the committed fixture.
#   4. Copies the resulting single .db file to testdata/fixtures/gitmoot-v0.9.0.db.
#
# Requirements: `gh` (authenticated, for the release download) and `go` (to
# run the checkpoint/vacuum step through modernc.org/sqlite -- the same
# driver gitmoot itself uses, so the on-disk format is guaranteed compatible).
#
# GOOS/GOARCH note: the release asset name is platform-specific
# (gitmoot_<os>_<arch>, e.g. gitmoot_linux_arm64, gitmoot_darwin_arm64). Pick
# the asset matching the host this script runs on.

set -euo pipefail

REPO="jerryfane/gitmoot"
VERSION="v0.9.0"
ASSET="${GITMOOT_FIXTURE_ASSET:-gitmoot_linux_arm64}"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

FIXTURE_HOME="$WORKDIR/fixturehome"
REPO_CHECKOUT="$WORKDIR/fixture-repo"
GOAL_FILE="$WORKDIR/fixture-goal.md"
mkdir -p "$FIXTURE_HOME"

echo "==> downloading $ASSET from $REPO@$VERSION" >&2
for attempt in 1 2 3 4 5; do
	if gh release download "$VERSION" --repo "$REPO" --pattern "$ASSET" --dir "$WORKDIR" --clobber; then
		break
	fi
	if [ "$attempt" -eq 5 ]; then
		echo "gh release download failed after $attempt attempts (api.github.com is known to be flaky here)" >&2
		exit 1
	fi
	sleep $((attempt * 3))
done
BIN="$WORKDIR/$ASSET"
chmod +x "$BIN"

echo "==> init" >&2
"$BIN" init -home "$FIXTURE_HOME"

echo "==> creating throwaway local git checkout to register as the tracked repo" >&2
git init -q -b main "$REPO_CHECKOUT"
git -C "$REPO_CHECKOUT" config user.email "fixture@example.com"
git -C "$REPO_CHECKOUT" config user.name "Fixture Gen"
echo "fixture repo" >"$REPO_CHECKOUT/README.md"
git -C "$REPO_CHECKOUT" add README.md
git -C "$REPO_CHECKOUT" commit -q -m "init"
git -C "$REPO_CHECKOUT" remote add origin "https://github.com/fixtureorg/fixture-repo.git"

echo "==> repo add" >&2
"$BIN" repo add fixtureorg/fixture-repo --path "$REPO_CHECKOUT" -home "$FIXTURE_HOME"

cat >"$GOAL_FILE" <<'EOF'
# Fixture goal

Generate a real v0.9.0 fixture database for gitmoot correction packet W1-01.
This goal exists purely to exercise the goal/task/job code paths offline.

## Implementation Tasks

### Task 1: Add a NOTES file

Add a NOTES.md file documenting the fixture purpose.
EOF

echo "==> goal import" >&2
"$BIN" goal import --file "$GOAL_FILE" --repo fixtureorg/fixture-repo -home "$FIXTURE_HOME"

echo "==> agent start (codex, workspace-write -- no network, no real session)" >&2
"$BIN" agent start fixture-agent --runtime codex --repo fixtureorg/fixture-repo \
	--path "$REPO_CHECKOUT" --policy workspace-write -home "$FIXTURE_HOME"

echo "==> job open/close (implement) + job record (review) -- the external-coordinator" \
	"job paths, so no live runtime session is invoked" >&2
"$BIN" job open --agent fixture-agent --repo fixtureorg/fixture-repo --type implement \
	--task task-001 --title "Fixture job for W1-01" -home "$FIXTURE_HOME" --json >"$WORKDIR/job-open.json"
# jq-free extraction: job open --json's only "job_id" key, on its own line.
JOB_ID=$(grep -o '"job_id": *"[^"]*"' "$WORKDIR/job-open.json" | head -1 | cut -d'"' -f4)
"$BIN" job close "$JOB_ID" --decision implemented --summary "Fixture NOTES.md added" \
	--branch task-001-add-a-notes-file -home "$FIXTURE_HOME"
"$BIN" job record --agent fixture-agent --repo fixtureorg/fixture-repo --type review \
	--decision approved --task task-001 --title "Fixture review" --summary "Looks good" \
	-home "$FIXTURE_HOME"

DB="$FIXTURE_HOME/.gitmoot/gitmoot.db"

echo "==> checkpoint + vacuum (drop WAL, compact) before freezing" >&2
cat >"$WORKDIR/checkpoint.go" <<'EOF'
package main

import (
	"database/sql"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	db, err := sql.Open("sqlite", os.Args[1])
	if err != nil {
		panic(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		panic(err)
	}
	if _, err := db.Exec(`VACUUM`); err != nil {
		panic(err)
	}
}
EOF
( cd "$(dirname "$0")/.." && GOFLAGS=-buildvcs=false go run "$WORKDIR/checkpoint.go" "$DB" )
rm -f "$DB-wal" "$DB-shm"

DEST="$(cd "$(dirname "$0")/.." && pwd)/testdata/fixtures/gitmoot-v0.9.0.db"
cp "$DB" "$DEST"
echo "==> wrote $DEST ($(du -h "$DEST" | cut -f1))" >&2
