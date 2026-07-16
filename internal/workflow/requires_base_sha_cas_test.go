package workflow

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// fakeRemoteBaseResolver extends fakeWorktreeManager with the
// RemoteDefaultBranchResolver and BaseAncestryChecker capabilities
// AllocateTaskWorktree's requires_base_sha CAS (CRB-15) type-asserts for. It
// simulates a checkout whose REMOTE (post-fetch) default branch resolves to
// remoteSHA, independent of whatever the caller's stale local BaseBranch might
// have implied, so a test can assert gitmoot consults the fetched remote value
// rather than any cached local state.
type fakeRemoteBaseResolver struct {
	fakeWorktreeManager
	fetchCalls    []string // remote names FetchRemote was invoked with
	fetchErr      error
	revParseCalls []string // rev strings RevParse was invoked with
	remoteSHA     string   // what RevParse resolves ANY ref to (the "real fetch" result)
	revParseErr   error
	ancestorCalls [][2]string // [ancestor, descendant] pairs IsAncestor was invoked with
	isAncestor    bool
	ancestorErr   error
}

func (f *fakeRemoteBaseResolver) FetchRemote(_ context.Context, remote string) error {
	f.fetchCalls = append(f.fetchCalls, remote)
	return f.fetchErr
}

func (f *fakeRemoteBaseResolver) RevParse(_ context.Context, rev string) (string, error) {
	f.revParseCalls = append(f.revParseCalls, rev)
	if f.revParseErr != nil {
		return "", f.revParseErr
	}
	return f.remoteSHA, nil
}

func (f *fakeRemoteBaseResolver) IsAncestor(_ context.Context, ancestor string, descendant string) (bool, error) {
	f.ancestorCalls = append(f.ancestorCalls, [2]string{ancestor, descendant})
	if f.ancestorErr != nil {
		return false, f.ancestorErr
	}
	return f.isAncestor, nil
}

// 40-hex-char (SHA-1-shaped) fixture SHAs. Values are arbitrary; only their
// distinctness and length matter to the fakes below.
var (
	shaDependencyA = strings.Repeat("a", 40)
	shaStaleMain   = strings.Repeat("1", 40)
	shaFreshMain   = strings.Repeat("2", 40)
)

// TestStaleBaseRejectedBeforeAnyAllocation is packet W2-05's regression test
// for CRB-15 ("a new root can use stale main after a dependency merge"). It
// pins the requires_base_sha CAS: dependency A merged, but the remote default
// branch a REAL fetch would observe (remoteSHA) is NOT a descendant of A
// (simulating the CRB-15 gap -- the allocator must fail, not silently fall
// back to whatever stale ref the caller happened to pass as BaseBranch).
// Allocation must fail closed with NO task row, NO branch lock, and NO
// worktree/job created.
//
// This cannot pass on base: TaskWorktreeRequest has no RequiresBaseSHA /
// DefaultBranchRef fields and AllocateTaskWorktree performs no remote fetch or
// ancestry check at all on base, so it always allocates unconditionally from
// the caller-supplied (possibly stale-cached) BaseBranch -- exactly CRB-15
// ("Run 2 ... still used the pre-Run-1 main parent as base").
func TestStaleBaseRejectedBeforeAnyAllocation(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	home := t.TempDir()
	checkout := t.TempDir()

	resolver := &fakeRemoteBaseResolver{
		remoteSHA:  shaStaleMain, // what a real fetch+resolve of the remote default branch returns
		isAncestor: false,        // shaDependencyA is NOT an ancestor of shaStaleMain: main is genuinely stale
	}

	task, err := engine.AllocateTaskWorktree(ctx, TaskWorktreeRequest{
		Home:             home,
		Repo:             "owner/repo",
		GoalID:           "goal-1",
		TaskID:           "task-b",
		TaskTitle:        "Dependent task B",
		Branch:           "task-b",
		BaseBranch:       "main", // even though the caller thinks main is fine, only a real fetch counts
		Owner:            "lead",
		Checkout:         checkout,
		RequiresBaseSHA:  shaDependencyA,
		DefaultBranchRef: "main",
	}, resolver)

	if err == nil {
		t.Fatalf("AllocateTaskWorktree succeeded despite unmet requires_base_sha; task=%+v", task)
	}
	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("AllocateTaskWorktree error = %v (%T), want a BlockedError", err, err)
	}

	// The remote MUST have actually been fetched and resolved -- never trusting
	// the caller's BaseBranch ("main") as if it already reflected the merge.
	if len(resolver.fetchCalls) != 1 || resolver.fetchCalls[0] != "origin" {
		t.Fatalf("FetchRemote calls = %+v, want exactly one call for origin", resolver.fetchCalls)
	}
	if len(resolver.revParseCalls) != 1 || resolver.revParseCalls[0] != "origin/main^{commit}" {
		t.Fatalf("RevParse calls = %+v, want exactly one call resolving origin/main^{commit}", resolver.revParseCalls)
	}
	if len(resolver.ancestorCalls) != 1 || resolver.ancestorCalls[0] != [2]string{shaDependencyA, shaStaleMain} {
		t.Fatalf("IsAncestor calls = %+v, want exactly one call (%s, %s)", resolver.ancestorCalls, shaDependencyA, shaStaleMain)
	}

	// NO task/worktree/job row may exist: the CAS must fail BEFORE any
	// allocation, not roll one back after the fact.
	if _, terr := store.GetTask(ctx, "task-b"); !errors.Is(terr, sql.ErrNoRows) {
		t.Fatalf("GetTask after rejected allocation error = %v, want sql.ErrNoRows", terr)
	}
	if _, lerr := store.GetBranchLock(ctx, "owner/repo", "task-b"); !errors.Is(lerr, sql.ErrNoRows) {
		t.Fatalf("GetBranchLock after rejected allocation error = %v, want sql.ErrNoRows", lerr)
	}
	if len(resolver.calls) != 0 || len(resolver.existingCalls) != 0 {
		t.Fatalf("worktree AddWorktree/AddExistingBranchWorktree calls = add:%+v existing:%+v, want none", resolver.calls, resolver.existingCalls)
	}
}

// TestFreshBaseAllocatesNormally is W2-05's positive-path proof: task B,
// launched with requires_base_sha pinned to dependency A's SHA, allocates
// successfully once a real fetch observes a remote default branch that
// descends from A -- with the SAME worktree/branch/task-row behavior as the
// unconstrained path (zero behavior change other than the extra fetch +
// ancestry check and the persisted allocated_base_sha).
func TestFreshBaseAllocatesNormally(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	home := t.TempDir()
	checkout := t.TempDir()

	resolver := &fakeRemoteBaseResolver{
		remoteSHA:  shaFreshMain, // the remote now includes dependency A's merge
		isAncestor: true,
	}

	task, err := engine.AllocateTaskWorktree(ctx, TaskWorktreeRequest{
		Home:             home,
		Repo:             "owner/repo",
		GoalID:           "goal-1",
		TaskID:           "task-b",
		TaskTitle:        "Dependent task B",
		Branch:           "task-b",
		BaseBranch:       "main",
		Owner:            "lead",
		Checkout:         checkout,
		RequiresBaseSHA:  shaDependencyA,
		DefaultBranchRef: "main",
	}, resolver)
	if err != nil {
		t.Fatalf("AllocateTaskWorktree returned error: %v", err)
	}
	if task.WorktreePath == "" || task.Branch != "task-b" {
		t.Fatalf("allocated task = %+v, want a populated worktree path and branch task-b", task)
	}
	if task.AllocatedBaseSHA != shaFreshMain {
		t.Fatalf("task.AllocatedBaseSHA = %q, want %q", task.AllocatedBaseSHA, shaFreshMain)
	}
	if len(resolver.calls) != 1 || resolver.calls[0].branch != "task-b" || resolver.calls[0].base != "main" {
		t.Fatalf("AddWorktree calls = %+v, want one call for branch task-b base main", resolver.calls)
	}
	stored, err := store.GetTask(ctx, "task-b")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if stored.AllocatedBaseSHA != shaFreshMain {
		t.Fatalf("persisted AllocatedBaseSHA = %q, want %q", stored.AllocatedBaseSHA, shaFreshMain)
	}
}

// TestUnconstrainedAllocationSkipsRemoteResolution pins that a request with no
// RequiresBaseSHA never triggers a fetch/resolve/ancestry check at all -- the
// unconstrained launch path (the overwhelming majority of callers) is
// byte-for-byte unchanged by CRB-15's fix. It uses a bare fakeWorktreeManager,
// which implements neither RemoteDefaultBranchResolver nor
// BaseAncestryChecker, so any attempt to invoke that path would fail loudly
// via the type-assertion error rather than silently no-op.
func TestUnconstrainedAllocationSkipsRemoteResolution(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	home := t.TempDir()
	checkout := t.TempDir()

	manager := &fakeWorktreeManager{}
	task, err := engine.AllocateTaskWorktree(ctx, TaskWorktreeRequest{
		Home:       home,
		Repo:       "owner/repo",
		GoalID:     "goal-1",
		TaskID:     "task-unconstrained",
		TaskTitle:  "No dependency",
		Branch:     "task-unconstrained",
		BaseBranch: "main",
		Owner:      "lead",
		Checkout:   checkout,
	}, manager)
	if err != nil {
		t.Fatalf("AllocateTaskWorktree returned error: %v", err)
	}
	if task.AllocatedBaseSHA != "" {
		t.Fatalf("AllocatedBaseSHA = %q, want empty for an unconstrained allocation", task.AllocatedBaseSHA)
	}
}

// TestRequiresBaseSHADefaultBranchAcceptsSlash pins the CRB-15 default-branch-
// name requirement: a default branch name containing '/' (e.g. the common
// "release/next" pattern) must resolve and validate normally rather than
// being rejected by an over-narrow branch-name check.
func TestRequiresBaseSHADefaultBranchAcceptsSlash(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	home := t.TempDir()
	checkout := t.TempDir()

	resolver := &fakeRemoteBaseResolver{
		remoteSHA:  shaFreshMain,
		isAncestor: true,
	}
	_, err := engine.AllocateTaskWorktree(ctx, TaskWorktreeRequest{
		Home:             home,
		Repo:             "owner/repo",
		GoalID:           "goal-1",
		TaskID:           "task-slash",
		TaskTitle:        "Slash default branch",
		Branch:           "task-slash",
		BaseBranch:       "release/next",
		Owner:            "lead",
		Checkout:         checkout,
		RequiresBaseSHA:  shaDependencyA,
		DefaultBranchRef: "release/next",
	}, resolver)
	if err != nil {
		t.Fatalf("AllocateTaskWorktree returned error for default branch %q: %v", "release/next", err)
	}
	if len(resolver.revParseCalls) != 1 || resolver.revParseCalls[0] != "origin/release/next^{commit}" {
		t.Fatalf("RevParse calls = %+v, want exactly one call resolving origin/release/next^{commit}", resolver.revParseCalls)
	}
	if _, err := store.GetTask(ctx, "task-slash"); err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
}
