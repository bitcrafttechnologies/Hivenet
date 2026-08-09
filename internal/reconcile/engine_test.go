package reconcile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bitcrafttech/hivenet/internal/driver/fake"
	"github.com/bitcrafttech/hivenet/internal/store"
	"github.com/bitcrafttech/hivenet/internal/topology"
)

func testEngine(t *testing.T, debounce time.Duration) (*Engine, *fake.Driver, *store.Store) {
	t.Helper()
	drv := fake.New()
	st := store.New()
	eng := NewEngine(Options{
		Driver:   drv,
		Store:    st,
		Debounce: debounce,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return eng, drv, st
}

func pair() topology.Topology {
	return topology.Topology{
		Nodes: []topology.Node{node("h1"), node("h2")},
		Links: []topology.Link{link("l1", "h1", "h2")},
	}
}

func mustReplace(t *testing.T, st *store.Store, topo topology.Topology) {
	t.Helper()
	if _, _, err := st.Replace(topo); err != nil {
		t.Fatalf("store.Replace: %v", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func TestReconcileConverges(t *testing.T) {
	eng, drv, st := testEngine(t, time.Second)
	mustReplace(t, st, pair())

	status := eng.ReconcileNow(context.Background())

	if status.Error != "" {
		t.Fatalf("unexpected cycle error: %s", status.Error)
	}
	for _, id := range []string{"h1", "h2"} {
		if got := status.Nodes[id].State; got != StateUp {
			t.Errorf("node %s state = %q (%s), want up", id, got, status.Nodes[id].Message)
		}
	}
	if got := status.Links["l1"].State; got != StateUp {
		t.Errorf("link l1 state = %q (%s), want up", got, status.Links["l1"].Message)
	}

	actual, err := drv.ReadActual(context.Background(), topology.DefaultOwner)
	if err != nil {
		t.Fatalf("ReadActual: %v", err)
	}
	if len(actual.Nodes) != 2 || len(actual.Links) != 1 {
		t.Errorf("driver holds %d nodes and %d links, want 2 and 1", len(actual.Nodes), len(actual.Links))
	}
}

// Re-running a converged reconcile must be a no-op. This is what makes the
// debounced loop safe to fire as often as it likes (spec §5.4).
func TestReconcileIsIdempotent(t *testing.T) {
	eng, drv, st := testEngine(t, time.Second)
	mustReplace(t, st, pair())
	eng.ReconcileNow(context.Background())

	drv.ResetCalls()
	eng.ReconcileNow(context.Background())

	if calls := drv.Calls(); len(calls) != 0 {
		t.Errorf("second reconcile issued %v, want no ops", calls)
	}
}

// Spec §5.5: no rollback. A failed node must not undo work that already
// succeeded — "fix the red node, re-run" is the whole v1 recovery model.
func TestPartialFailureLeavesAppliedWorkInPlace(t *testing.T) {
	eng, drv, st := testEngine(t, time.Second)
	drv.FailOp("CreateNode:h2", errors.New("image pull failed"))
	mustReplace(t, st, pair())

	status := eng.ReconcileNow(context.Background())

	if got := status.Nodes["h1"].State; got != StateUp {
		t.Errorf("h1 state = %q, want up: a healthy node must not be rolled back", got)
	}
	if got := status.Nodes["h2"].State; got != StateError {
		t.Errorf("h2 state = %q, want error", got)
	}
	if status.Nodes["h2"].Message == "" {
		t.Error("failed node must carry a message explaining the failure")
	}
	// The link cannot exist without both endpoints, so it fails too.
	if got := status.Links["l1"].State; got != StateError {
		t.Errorf("link l1 state = %q, want error", got)
	}

	actual, _ := drv.ReadActual(context.Background(), topology.DefaultOwner)
	if len(actual.Nodes) != 1 || actual.Nodes[0].ID != "h1" {
		t.Errorf("expected h1 to survive the partial failure, driver holds %+v", actual.Nodes)
	}
}

// Recovery after a container dies on its own: the next cycle reads actual state,
// sees the gap and refills it — including the link, whose veth died with the netns.
func TestReconcileRebuildsNodeThatDiedOnItsOwn(t *testing.T) {
	eng, drv, st := testEngine(t, time.Second)
	mustReplace(t, st, pair())
	eng.ReconcileNow(context.Background())

	drv.RemoveNode("h1")
	drv.ResetCalls()

	status := eng.ReconcileNow(context.Background())

	calls := drv.Calls()
	if !contains(calls, "CreateNode:h1") {
		t.Errorf("expected the dead node to be recreated, got %v", calls)
	}
	if !contains(calls, "CreateLink:l1") {
		t.Errorf("expected the link to be rebuilt with the node, got %v", calls)
	}
	if got := status.Nodes["h1"].State; got != StateUp {
		t.Errorf("h1 state = %q, want up after recovery", got)
	}
}

func TestReconcileRemovesUnmanagedLeftovers(t *testing.T) {
	eng, drv, st := testEngine(t, time.Second)
	mustReplace(t, st, pair())
	eng.ReconcileNow(context.Background())

	drv.InjectNode(topology.DefaultOwner, node("ghost"))
	drv.ResetCalls()

	eng.ReconcileNow(context.Background())

	if !contains(drv.Calls(), "DestroyNode:ghost") {
		t.Errorf("expected the orphan to be destroyed, got %v", drv.Calls())
	}
}

// Spec §5.6: drift is surfaced, never silently corrected. When a destroy fails,
// the leftover has to remain visible rather than vanish from the status.
func TestUnremovableOrphanIsSurfacedAsDrift(t *testing.T) {
	eng, drv, st := testEngine(t, time.Second)
	mustReplace(t, st, pair())
	eng.ReconcileNow(context.Background())

	drv.InjectNode(topology.DefaultOwner, node("ghost"))
	drv.FailOp("DestroyNode:ghost", errors.New("device or resource busy"))

	status := eng.ReconcileNow(context.Background())

	ghost, reported := status.Nodes["ghost"]
	if !reported {
		t.Fatal("an orphan that could not be destroyed must still appear in status")
	}
	if !ghost.Drift {
		t.Error("orphan must be flagged as drift")
	}
	if ghost.State != StateError {
		t.Errorf("orphan state = %q, want error", ghost.State)
	}
}

func TestReadActualFailureIsReportedAtCycleLevel(t *testing.T) {
	eng, drv, st := testEngine(t, time.Second)
	mustReplace(t, st, pair())
	drv.FailOp("ReadActual:", errors.New("podman socket unreachable"))

	status := eng.ReconcileNow(context.Background())

	if status.Error == "" {
		t.Fatal("expected a cycle-level error when actual state cannot be read")
	}
	if len(drv.Calls()) != 0 {
		t.Errorf("no ops should be attempted when actual state is unknown, got %v", drv.Calls())
	}
}

func TestAdoptSeedsStoreFromRunningState(t *testing.T) {
	eng, drv, st := testEngine(t, time.Second)
	drv.InjectNode(topology.DefaultOwner, node("survivor"))

	if err := eng.Adopt(context.Background()); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	doc := st.Get()
	if len(doc.Topology.Nodes) != 1 || doc.Topology.Nodes[0].ID != "survivor" {
		t.Fatalf("expected the running node to be adopted, store holds %+v", doc.Topology.Nodes)
	}

	// The adopted state must already be converged: adopting then reconciling
	// must not tear down the lab it just took over.
	drv.ResetCalls()
	eng.ReconcileNow(context.Background())
	if calls := drv.Calls(); len(calls) != 0 {
		t.Errorf("reconcile after adopt issued %v, want no ops", calls)
	}
}

func TestAdoptLeavesAPopulatedStoreAlone(t *testing.T) {
	eng, drv, st := testEngine(t, time.Second)
	mustReplace(t, st, pair())
	drv.InjectNode(topology.DefaultOwner, node("survivor"))

	if err := eng.Adopt(context.Background()); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	if doc := st.Get(); len(doc.Topology.Nodes) != 2 {
		t.Errorf("Adopt overwrote a store that already held desired state: %+v", doc.Topology.Nodes)
	}
}

// Spec §5.7: edits arriving during the debounce window collapse into one cycle
// against the newest desired state — intermediate states are never applied.
func TestRunCoalescesRapidEdits(t *testing.T) {
	eng, drv, st := testEngine(t, 80*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = eng.Run(ctx) }()

	for _, id := range []string{"first", "second", "third"} {
		mustReplace(t, st, topology.Topology{Nodes: []topology.Node{node(id)}})
	}

	waitFor(t, 3*time.Second, "the final topology to converge", func() bool {
		actual, err := drv.ReadActual(ctx, topology.DefaultOwner)
		return err == nil && len(actual.Nodes) == 1 && actual.Nodes[0].ID == "third"
	})

	calls := drv.Calls()
	for _, skipped := range []string{"CreateNode:first", "CreateNode:second"} {
		if contains(calls, skipped) {
			t.Errorf("intermediate edit %q was applied; edits should coalesce. calls: %v", skipped, calls)
		}
	}
}

func TestSubscribeReceivesStatusUpdates(t *testing.T) {
	eng, _, st := testEngine(t, time.Second)
	updates, unsubscribe := eng.Subscribe()
	defer unsubscribe()

	mustReplace(t, st, pair())
	eng.ReconcileNow(context.Background())

	select {
	case status := <-updates:
		if len(status.Nodes) != 2 {
			t.Errorf("status carried %d nodes, want 2", len(status.Nodes))
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber received no status update")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
