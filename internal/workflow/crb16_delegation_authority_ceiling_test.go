package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

// This file is the regression suite for CRB-16: before
// validateDelegationAuthorityCeiling existed, preflightDelegation validated only
// the TARGET of a delegation (is the named agent registered / repo-scoped /
// capable, or is the ephemeral spec well-formed) and never asked whether the
// REQUESTING parent's own authority permitted it to hand out that capability. A
// parent whose registered agent lacks "implement" could therefore name a
// legitimately registered "implement" agent, or bootstrap a brand-new ephemeral
// worker with a writable autonomy policy, and the daemon would materialize it — a
// confused deputy escalating a read-only seat to write access via its own
// delegation channel.
//
// The discriminator is the PARENT'S OWN AGENT REGISTRATION (agents table row
// named by parent.Agent — its capabilities_json), NOT the parent job's Type. An
// earlier attempt at this fix gated on job Type (ask/review vs. implement) and
// was rejected: production coordinators legitimately run AS ask-type jobs while
// their registered agent carries "implement" in its own capability set (the
// council-lead-codex pattern: an ask-type job whose agent is registered
// ["ask","review","implement"], delegating implement children from its R3
// phase) — gating on job Type broke that primary flow. Every test below
// therefore keeps its parent job Type as whatever it would naturally be
// ("ask") and varies the parent AGENT's registered capabilities instead.
//
// FAILS ON BASE (pre-CRB-16, i.e. with validateDelegationAuthorityCeiling and its
// call site removed from preflightDelegation): every "Cannot" test below
// dispatches the child successfully instead of being refused, because
// preflightDelegation's target-only checks (agent exists, repo-scoped, has the
// requested capability; ephemeral spec is well-formed) have nothing to say about
// the PARENT's own registered capabilities — "builder"/"worker" are entirely
// valid targets, so the pre-fix code approves them. Only the ceiling added for
// CRB-16 makes them REJECT.

// TestCRB16ReviewParentCannotDelegateNamedImplementChild is the primary negative
// proof mandated by the CRB-16 packet: a parent whose registered agent lacks
// "implement" naming a registered, in-repo, implement-capable agent must be
// REJECTED with the CRB-16 ceiling error (naming the parent job id, the parent's
// AGENT and its registered capabilities, and the refused child action), and the
// refused delegation must leave no durable side effect — no child job row and no
// branch lock.
func TestCRB16ReviewParentCannotDelegateNamedImplementChild(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	// "reviewer" is registered WITHOUT "implement" — exactly the
	// council-glm/council-gate production shape (["ask","review"]).
	seedAgent(t, store, "reviewer", []string{"ask", "review"}, "jerryfane/gitmoot")
	// builder is a perfectly legitimate implement target: registered, repo-scoped,
	// and capable. Pre-CRB-16 that is all preflightDelegation checked.
	seedAgent(t, store, "builder", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "reviewer", Type: "review"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		Sender: "reviewer",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "esc", Agent: "builder", Action: "implement", Prompt: "write code"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	reason := preflightFailureReason(t, store, "parent-job")
	for _, want := range []string{
		"CRB-16",
		`parent job "parent-job"`,
		`agent "reviewer"`,
		`capabilities [ask, review]`,
		`a "implement" child`,
		`do not include "implement"`,
	} {
		if !strings.Contains(reason, want) {
			t.Fatalf("preflight reason %q missing %q", reason, want)
		}
	}
	if jobExists(t, store, "parent-job/delegation/esc") {
		t.Fatalf("denied delegation must create no child job row")
	}
	if got := countBranchLocks(t, store, "jerryfane/gitmoot"); got != 0 {
		t.Fatalf("denied delegation must create no branch lock, got %d", got)
	}
}

// TestCRB16AskParentCannotDelegateNamedImplementChild is the "ask" half of the
// SCOPE RULING: an ask-TYPE parent whose registered agent lacks "implement" is
// refused exactly like a review-type parent in the same situation — job Type
// plays no role, only the parent's own agent registration does.
func TestCRB16AskParentCannotDelegateNamedImplementChild(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	// "asker" is registered WITHOUT "implement" — the narrower
	// council-glm/council-gate shape (["ask"] only).
	seedAgent(t, store, "asker", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "builder", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "asker", Type: "ask"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		Sender: "asker",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "esc", Agent: "builder", Action: "implement", Prompt: "write code"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	reason := preflightFailureReason(t, store, "parent-job")
	for _, want := range []string{
		"CRB-16",
		`parent job "parent-job"`,
		`agent "asker"`,
		`capabilities [ask]`,
		`a "implement" child`,
		`do not include "implement"`,
	} {
		if !strings.Contains(reason, want) {
			t.Fatalf("preflight reason %q missing %q", reason, want)
		}
	}
	if jobExists(t, store, "parent-job/delegation/esc") {
		t.Fatalf("denied delegation must create no child job row")
	}
	if got := countBranchLocks(t, store, "jerryfane/gitmoot"); got != 0 {
		t.Fatalf("denied delegation must create no branch lock, got %d", got)
	}
}

// TestCRB16AskParentCannotBootstrapWritableEphemeralChild covers the second
// escalation path CRB-16 documents: an ephemeral child whose DECLARED action is
// itself ask/review (so it never trips the implement-capability check above) but
// whose autonomy policy grants write (workspace-write / danger-full-access). The
// daemon's write-policy guard (ImplementWritePolicyError) is implement-capability
// specific and never inspects this, so before CRB-16 a parent whose agent lacks
// "implement" could bootstrap a nominally "ask" ephemeral worker with
// danger-full-access and get a full Bash/file-write agent under a read-only
// label.
func TestCRB16AskParentCannotBootstrapWritableEphemeralChild(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coordinator", []string{"ask"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coordinator", Type: "ask"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		Sender: "coordinator",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{
					ID:        "worker",
					Ephemeral: &EphemeralSpec{Runtime: "codex", AutonomyPolicy: "danger-full-access"},
					Action:    "ask",
					Prompt:    "look around",
				},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	reason := preflightFailureReason(t, store, "parent-job")
	for _, want := range []string{
		"CRB-16",
		`parent job "parent-job"`,
		`agent "coordinator"`,
		`capabilities [ask]`,
		`ephemeral "ask" child`,
		`autonomy policy "danger-full-access"`,
		`do not include "implement"`,
	} {
		if !strings.Contains(reason, want) {
			t.Fatalf("preflight reason %q missing %q", reason, want)
		}
	}
	if jobExists(t, store, "parent-job/delegation/worker") {
		t.Fatalf("denied delegation must create no child job row")
	}
	if got := countBranchLocks(t, store, "jerryfane/gitmoot"); got != 0 {
		t.Fatalf("denied delegation must create no branch lock, got %d", got)
	}
}

// TestCRB16ImplementParentCanStillDelegateImplementChildren is a positive-path
// proof the packet requires: a parent whose registered agent has "implement" and
// whose job Type is itself "implement" is completely unaffected by the new
// ceiling.
func TestCRB16ImplementParentCanStillDelegateImplementChildren(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coordinator", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "builder", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coordinator", Type: "implement"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		Branch: "task-005",
		Sender: "coordinator",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "impl", Agent: "builder", Action: "implement", Prompt: "build it"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	child := mustJob(t, store, "parent-job/delegation/impl")
	if child.Agent != "builder" || child.State != string(JobQueued) {
		t.Fatalf("implement-parent's implement child = %+v, want dispatched", child)
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_preflight_failed"); got != 0 {
		t.Fatalf("a legitimate implement-parent delegation must not trip the ceiling, got %d preflight failures", got)
	}
}

// TestCRB16AskJobWithImplementCapableAgentCanDelegateImplementChild is the
// council-lead-codex production-pattern proof this packet exists to preserve: a
// parent job typed "ask" (a coordinator's R-phase, e.g. council-lead-codex's R3)
// whose REGISTERED AGENT nonetheless carries "implement" in its capabilities
// (["ask","review","implement"], the coordinator shape) must successfully
// delegate an implement child. This is exactly the flow the rejected job-Type-only
// attempt broke: it gated on job.Type == "ask" and refused this, even though the
// delegating agent was fully authorized to hand out "implement". This test FAILS
// on that attempt and PASSES under the agent-capability discriminator.
func TestCRB16AskJobWithImplementCapableAgentCanDelegateImplementChild(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	// council-lead-codex production shape: ["ask","review","implement"].
	seedAgent(t, store, "council-lead-codex", []string{"ask", "review", "implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "builder", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	// The driving job is Type "ask" — the coordinator's own root job type never
	// changes to "implement" even while it fans out implement children.
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "council-lead-codex", Type: "ask"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		Branch: "task-005",
		Sender: "council-lead-codex",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "R3: implement",
			Delegations: []Delegation{
				{ID: "impl", Agent: "builder", Action: "implement", Prompt: "build it"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	child := mustJob(t, store, "parent-job/delegation/impl")
	if child.Agent != "builder" || child.State != string(JobQueued) {
		t.Fatalf("ask-job/implement-capable-agent's implement child = %+v, want dispatched", child)
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_preflight_failed"); got != 0 {
		t.Fatalf("council-lead-codex's ask-typed R3 implement delegation must not trip the ceiling, got %d preflight failures", got)
	}
}

// TestCRB16ReviewParentCanStillDelegateReviewAndAskChildren is the second
// positive-path proof the packet requires: a parent whose registered agent lacks
// "implement" can still delegate review/ask children — both a named registered
// agent and a read-only ephemeral worker — completely unaffected by the new
// ceiling (only "implement" and write-granting ephemeral delegations are gated).
func TestCRB16ReviewParentCanStillDelegateReviewAndAskChildren(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "reviewer", []string{"ask", "review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "helper", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "reviewer", Type: "review"}, JobPayload{
		Repo:   "jerryfane/gitmoot",
		Sender: "reviewer",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "named", Agent: "helper", Action: "review", Prompt: "double-check"},
				// No AutonomyPolicy set: normalizes to "auto", which grants no
				// write, so this stays within the review parent's ceiling.
				{ID: "eph", Ephemeral: &EphemeralSpec{Runtime: "codex"}, Action: "ask", Prompt: "look around"},
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	namedChild := mustJob(t, store, "parent-job/delegation/named")
	if namedChild.Agent != "helper" || namedChild.State != string(JobQueued) {
		t.Fatalf("review-parent's named review child = %+v, want dispatched", namedChild)
	}
	ephChild := mustJob(t, store, "parent-job/delegation/eph")
	if !strings.Contains(ephChild.Agent, "-ephemeral-") || ephChild.State != string(JobQueued) {
		t.Fatalf("review-parent's read-only ephemeral child = %+v, want dispatched", ephChild)
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_preflight_failed"); got != 0 {
		t.Fatalf("a legitimate review-parent delegation must not trip the ceiling, got %d preflight failures", got)
	}
}
