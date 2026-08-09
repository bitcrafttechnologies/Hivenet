package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bitcrafttech/hivenet/internal/driver"
	"github.com/bitcrafttech/hivenet/internal/store"
	"github.com/bitcrafttech/hivenet/internal/topology"
)

// DefaultDebounce is the settle time after the last canvas or panel edit before
// a reconcile runs (spec §5.7 calls for 300–500ms).
const DefaultDebounce = 400 * time.Millisecond

// Engine is the single writer for all driver state (spec §5).
//
// Every podman and netlink call in the system happens on the one goroutine
// running Run. HTTP handlers mutate the store and return immediately; the
// engine converges. Because a cycle always re-reads the latest desired state
// from the store, edits arriving mid-apply coalesce naturally: the next cycle
// sees the final state, not every intermediate keystroke.
type Engine struct {
	drv      driver.Driver
	store    *store.Store
	owner    string
	debounce time.Duration
	log      *slog.Logger

	trigger chan struct{}

	// changes is subscribed in NewEngine rather than in Run. Subscribing here
	// means an edit landing between construction and the start of Run still
	// wakes the loop; subscribing inside Run would drop it.
	changes     <-chan struct{}
	unsubscribe func()

	mu      sync.RWMutex
	status  Status
	subs    map[int]chan Status
	nextSub int
}

// Options configures an Engine. Driver and Store are required.
type Options struct {
	Driver   driver.Driver
	Store    *store.Store
	Owner    string
	Debounce time.Duration
	Logger   *slog.Logger
}

// NewEngine builds an Engine. It does not start it; call Run.
func NewEngine(opts Options) *Engine {
	if opts.Owner == "" {
		opts.Owner = topology.DefaultOwner
	}
	if opts.Debounce <= 0 {
		opts.Debounce = DefaultDebounce
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	e := &Engine{
		drv:      opts.Driver,
		store:    opts.Store,
		owner:    opts.Owner,
		debounce: opts.Debounce,
		log:      opts.Logger,
		trigger:  make(chan struct{}, 1),
		status:   newStatus(0),
		subs:     make(map[int]chan Status),
	}
	e.changes, e.unsubscribe = opts.Store.Subscribe()
	return e
}

// Run drives reconcile cycles until ctx is cancelled. It returns ctx.Err().
//
// This goroutine is the single writer. Nothing else may call the driver's
// mutating methods.
func (e *Engine) Run(ctx context.Context) error {
	defer e.unsubscribe()

	timer := time.NewTimer(e.debounce)
	if !timer.Stop() {
		<-timer.C
	}
	armed := false

	arm := func() {
		if armed && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(e.debounce)
		armed = true
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.changes:
			arm()
		case <-e.trigger:
			arm()
		case <-timer.C:
			armed = false
			e.ReconcileNow(ctx)
		}
	}
}

// Trigger requests a reconcile after the debounce interval. It never blocks: a
// request arriving while one is already pending is absorbed, since a cycle
// always reads the newest desired state anyway.
func (e *Engine) Trigger() {
	select {
	case e.trigger <- struct{}{}:
	default:
	}
}

// Adopt seeds the store from state already running on the machine.
//
// Hivenet's store is in-memory, so without this a restart would come up with an
// empty desired state and immediately tear down a lab that is running perfectly
// well. Adopting instead means a restart of the app is not a restart of the
// lab. It only acts on an untouched store (version 0, no nodes).
func (e *Engine) Adopt(ctx context.Context) error {
	if doc := e.store.Get(); doc.Version != 0 || len(doc.Topology.Nodes) > 0 {
		return nil
	}
	actual, err := e.drv.ReadActual(ctx, e.owner)
	if err != nil {
		return fmt.Errorf("adopt existing state: %w", err)
	}
	if len(actual.Nodes) == 0 && len(actual.Links) == 0 {
		return nil
	}
	if _, _, err := e.store.Replace(actual); err != nil {
		return fmt.Errorf("adopt existing state: %w", err)
	}
	e.log.Info("adopted running topology", "nodes", len(actual.Nodes), "links", len(actual.Links))
	return nil
}

// ReconcileNow runs one full cycle synchronously and returns the resulting
// status. Run calls it on the debounce; tests and an explicit "Apply" action
// call it directly.
func (e *Engine) ReconcileNow(ctx context.Context) Status {
	doc := e.store.Get()

	// Read actual state first, always. In-memory state is never ground truth —
	// this is what lets a cycle recover after a crash or a VM reboot (spec §5.1).
	actual, err := e.drv.ReadActual(ctx, e.owner)
	if err != nil {
		return e.fail(doc, fmt.Errorf("read actual state: %w", err))
	}

	ops := Diff(doc.Topology, actual)
	if len(ops) == 0 {
		st := e.buildStatus(doc, actual, nil, failures{})
		e.setStatus(st)
		return st
	}

	e.log.Info("reconciling", "version", doc.Version, "ops", len(ops))
	e.setStatus(e.applyingStatus(doc, ops))

	failed := e.apply(ctx, doc.Topology, ops)

	// Re-read and re-diff rather than assuming the applied ops took. Anything
	// still outstanding is either a failure already recorded against an entity,
	// or genuine drift — and this is also the check that catches an op which
	// reported success without actually changing anything.
	post, err := e.drv.ReadActual(ctx, e.owner)
	if err != nil {
		return e.fail(doc, fmt.Errorf("verify state after apply: %w", err))
	}

	st := e.buildStatus(doc, post, Diff(doc.Topology, post), failed)
	e.setStatus(st)
	return st
}

// Status returns the most recent cycle result.
func (e *Engine) Status() Status {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.status.Clone()
}

// Subscribe returns a channel of status updates and an unsubscribe function.
//
// The channel holds only the newest status: a slow consumer — a browser on a
// bad connection — drops intermediate frames rather than stalling the engine.
// Status is a full snapshot, so dropping frames loses nothing.
func (e *Engine) Subscribe() (<-chan Status, func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := e.nextSub
	e.nextSub++
	ch := make(chan Status, 1)
	e.subs[id] = ch
	return ch, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if c, ok := e.subs[id]; ok {
			delete(e.subs, id)
			close(c)
		}
	}
}

// failures collects per-entity error messages from one apply pass.
type failures struct {
	nodes map[string]string
	links map[string]string
}

func newFailures() failures {
	return failures{nodes: make(map[string]string), links: make(map[string]string)}
}

func (e *Engine) apply(ctx context.Context, desired topology.Topology, ops []Op) failures {
	failed := newFailures()
	for _, op := range ops {
		if err := e.applyOp(ctx, desired, op); err != nil {
			// No rollback (spec §5.5). Record the failure against the entity
			// that caused it and keep going, so one bad node does not stop the
			// rest of the topology from converging.
			e.log.Error("reconcile op failed", "kind", op.Kind, "target", op.Target(), "error", err)
			msg := fmt.Sprintf("%s failed: %v", op.Kind, err)
			if op.NodeID != "" {
				failed.nodes[op.NodeID] = msg
			} else {
				failed.links[op.LinkID] = msg
			}
			continue
		}
		e.log.Debug("reconcile op applied", "kind", op.Kind, "target", op.Target(), "reason", op.Reason)
	}
	return failed
}

func (e *Engine) applyOp(ctx context.Context, desired topology.Topology, op Op) error {
	switch op.Kind {
	case OpCreateNode, OpUpdateNodeConfig:
		node, ok := desired.NodeByID(op.NodeID)
		if !ok {
			return fmt.Errorf("node %s is not in the desired topology", op.NodeID)
		}
		if op.Kind == OpCreateNode {
			return e.drv.CreateNode(ctx, node)
		}
		return e.drv.UpdateNodeConfig(ctx, node)

	case OpDestroyNode:
		return e.drv.DestroyNode(ctx, e.owner, op.NodeID)

	case OpCreateLink:
		link, ok := desired.LinkByID(op.LinkID)
		if !ok {
			return fmt.Errorf("link %s is not in the desired topology", op.LinkID)
		}
		return e.drv.CreateLink(ctx, e.owner, link)

	case OpDestroyLink:
		return e.drv.DestroyLink(ctx, e.owner, op.LinkID)

	default:
		return fmt.Errorf("unknown op kind %q", op.Kind)
	}
}

// applyingStatus marks every entity an op touches as applying, so the canvas
// can show a spinner for the duration of the cycle.
func (e *Engine) applyingStatus(doc topology.Document, ops []Op) Status {
	st := e.Status()
	st.Version = doc.Version
	st.UpdatedAt = time.Now()
	st.Error = ""
	for _, op := range ops {
		if op.NodeID != "" {
			st.Nodes[op.NodeID] = EntityStatus{ID: op.NodeID, State: StateApplying, Message: op.Reason}
		} else if op.LinkID != "" {
			st.Links[op.LinkID] = EntityStatus{ID: op.LinkID, State: StateApplying, Message: op.Reason}
		}
	}
	return st
}

// buildStatus produces the post-cycle picture from what is desired, what is
// actually there, what ops remain outstanding, and what failed.
func (e *Engine) buildStatus(doc topology.Document, actual topology.Topology, remaining []Op, failed failures) Status {
	st := newStatus(doc.Version)

	outstandingNodes := make(map[string]string)
	outstandingLinks := make(map[string]string)
	for _, op := range remaining {
		if op.NodeID != "" {
			if _, seen := outstandingNodes[op.NodeID]; !seen {
				outstandingNodes[op.NodeID] = op.Reason
			}
		} else if op.LinkID != "" {
			if _, seen := outstandingLinks[op.LinkID]; !seen {
				outstandingLinks[op.LinkID] = op.Reason
			}
		}
	}

	actualNodes := nodeIndex(actual.Nodes)
	for _, n := range doc.Topology.Nodes {
		es := EntityStatus{ID: n.ID, State: StateUp}
		_, running := actualNodes[n.ID]
		switch {
		case failed.nodes[n.ID] != "":
			es.State, es.Message = StateError, failed.nodes[n.ID]
		case !running:
			es.State, es.Message = StateError, "node is not running after reconcile"
		case outstandingNodes[n.ID] != "":
			es.Drift, es.Message = true, "drift: "+outstandingNodes[n.ID]
		}
		st.Nodes[n.ID] = es
	}
	for _, id := range sortedKeys(actualNodes) {
		if _, wanted := doc.Topology.NodeByID(id); wanted {
			continue
		}
		st.Nodes[id] = EntityStatus{
			ID:      id,
			State:   StateError,
			Drift:   true,
			Message: orDefault(failed.nodes[id], "node is running but is not in the desired topology"),
		}
	}

	actualLinks := linkIndex(actual.Links)
	for _, l := range doc.Topology.Links {
		es := EntityStatus{ID: l.ID, State: StateUp}
		_, up := actualLinks[l.ID]
		switch {
		case failed.links[l.ID] != "":
			es.State, es.Message = StateError, failed.links[l.ID]
		case !up:
			es.State, es.Message = StateError, "link does not exist after reconcile"
		case outstandingLinks[l.ID] != "":
			es.Drift, es.Message = true, "drift: "+outstandingLinks[l.ID]
		}
		st.Links[l.ID] = es
	}
	for _, id := range sortedKeys(actualLinks) {
		if _, wanted := doc.Topology.LinkByID(id); wanted {
			continue
		}
		st.Links[id] = EntityStatus{
			ID:      id,
			State:   StateError,
			Drift:   true,
			Message: orDefault(failed.links[id], "link exists but is not in the desired topology"),
		}
	}

	return st
}

// fail records a cycle-level failure — one that stopped the reconcile from
// running at all, as opposed to a per-entity error.
func (e *Engine) fail(doc topology.Document, err error) Status {
	e.log.Error("reconcile cycle failed", "version", doc.Version, "error", err)
	st := e.Status()
	st.Version = doc.Version
	st.UpdatedAt = time.Now()
	st.Error = err.Error()
	e.setStatus(st)
	return st
}

func (e *Engine) setStatus(st Status) {
	e.mu.Lock()
	e.status = st
	subs := make([]chan Status, 0, len(e.subs))
	for _, ch := range e.subs {
		subs = append(subs, ch)
	}
	e.mu.Unlock()

	for _, ch := range subs {
		// Keep only the newest status in each subscriber's slot.
		select {
		case ch <- st.Clone():
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- st.Clone():
			default:
			}
		}
	}
}

func orDefault(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
