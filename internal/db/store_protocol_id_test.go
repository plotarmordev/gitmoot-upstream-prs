package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// protocolIdentity reads back the two columns CreateJob/CreateJobWithEvent
// mint (council correction packet W1-02a). GetJob/ListJobs deliberately do
// NOT project these columns -- no reader needs them yet, and this packet's
// target files are scoped to the mint path only -- so tests read the raw
// columns directly the same way other internal/db tests reach columns that
// have no dedicated getter (e.g. TestBootIDStableAndCached's readBootID,
// advance_retry_test.go's direct store.db.Exec).
func protocolIdentity(t *testing.T, store *Store, jobID string) (taskID, attemptID string) {
	t.Helper()
	if err := store.db.QueryRow(`SELECT protocol_task_id, protocol_attempt_id FROM jobs WHERE id = ?`, jobID).Scan(&taskID, &attemptID); err != nil {
		t.Fatalf("query protocol identity for job %s: %v", jobID, err)
	}
	return taskID, attemptID
}

// TestCreateJobMintsProtocolIdentity pins W1-02a's regression case: every job
// row minted by CreateJob gets non-empty, opaque protocol_task_id/
// protocol_attempt_id values, and two independently-created jobs (even ones
// sharing the same root_id, i.e. the same delegation tree) get DISTINCT
// identity -- no accidental sharing through a package-level counter or a
// digest of shared input. Fails on current base: the jobs table has no
// protocol_task_id/protocol_attempt_id columns at all (the base migrations
// slice ends at W1-01's schema_migrations identity columns), so the INSERT
// in CreateJob naming these columns is a SQL error ("no such column"),
// structurally impossible to pass. Passes after: the W1-02a migration adds
// the two columns and CreateJob mints them via protocol.NewOpaqueID().
func TestCreateJobMintsProtocolIdentity(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.CreateJob(ctx, Job{ID: "root", Agent: "w", Type: "ask", State: "queued", Payload: "{}"}); err != nil {
		t.Fatalf("CreateJob(root) returned error: %v", err)
	}
	// Second job shares the first's root_id (payload.root_job_id = "root", the
	// engine's rootJobID() rule) but must still mint its OWN, distinct identity.
	if err := store.CreateJob(ctx, Job{ID: "root/delegation/d1", Agent: "w", Type: "ask", State: "queued", Payload: `{"root_job_id":"root"}`}); err != nil {
		t.Fatalf("CreateJob(child) returned error: %v", err)
	}

	rootTask, rootAttempt := protocolIdentity(t, store, "root")
	childTask, childAttempt := protocolIdentity(t, store, "root/delegation/d1")

	for name, id := range map[string]string{
		"root task_id": rootTask, "root attempt_id": rootAttempt,
		"child task_id": childTask, "child attempt_id": childAttempt,
	} {
		if id == "" {
			t.Fatalf("%s is empty, want a minted opaque id", name)
		}
		if strings.Contains(id, "/") {
			t.Fatalf("%s = %q, want opaque (no '/', unlike jobs.id)", name, id)
		}
	}

	if rootTask == childTask {
		t.Fatalf("root and child task_id both = %q, want distinct ids for two independently-created jobs", rootTask)
	}
	if rootAttempt == childAttempt {
		t.Fatalf("root and child attempt_id both = %q, want distinct ids for two independently-created jobs", rootAttempt)
	}
}

// TestProtocolIDSurvivesRetry pins the identity-spine half of DESIGN.md
// decision log #3: a retry mints an entirely new jobs.id (gitmoot's real
// retry shape, "<parent>/delegation/<id>/retry/<n>", engine.go:3147 via
// requeueDelegation) and therefore a new protocol_attempt_id, but the retry
// is the SAME logical unit of work as its predecessor, so protocol_task_id
// must carry forward unchanged. mintProtocolIdentity implements this by
// leaving an already-set ProtocolTaskID untouched; this test drives that
// contract the way a retry call site would, by reading the original's
// task_id back and setting it explicitly on the retry Job before calling
// CreateJob.
func TestProtocolIDSurvivesRetry(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	original := "parent/delegation/d1"
	if err := store.CreateJob(ctx, Job{ID: original, Agent: "w", Type: "ask", State: "failed", Payload: "{}"}); err != nil {
		t.Fatalf("CreateJob(original) returned error: %v", err)
	}
	originalTask, originalAttempt := protocolIdentity(t, store, original)

	retry := original + "/retry/1"
	if err := store.CreateJob(ctx, Job{ID: retry, Agent: "w", Type: "ask", State: "queued", Payload: "{}", ProtocolTaskID: originalTask}); err != nil {
		t.Fatalf("CreateJob(retry) returned error: %v", err)
	}
	retryTask, retryAttempt := protocolIdentity(t, store, retry)

	if retryTask != originalTask {
		t.Fatalf("retry task_id = %q, want SAME task_id as original %q", retryTask, originalTask)
	}
	if retryAttempt == "" || retryAttempt == originalAttempt {
		t.Fatalf("retry attempt_id = %q, want a NEW, non-empty id distinct from original %q", retryAttempt, originalAttempt)
	}
}
