// Package store holds the two documents the app is built around, kept
// deliberately separate (spec §4): the authoritative, versioned topology
// document that the reconciler acts on, and the client-owned layout document
// that it must never touch.
//
// v1 is in-memory only. Persistence would slot in behind these types without
// changing their callers.
package store

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/bitcrafttech/hivenet/internal/topology"
)

// Store is the authoritative topology document and the only place the version
// counter advances.
//
// It is safe for concurrent use. Every read returns a deep copy: callers get
// maps and slices they can mutate freely without corrupting stored state.
type Store struct {
	mu      sync.RWMutex
	doc     topology.Document
	subs    map[int]chan struct{}
	nextSub int
}

// New returns an empty store at version 0.
func New() *Store {
	return &Store{subs: make(map[int]chan struct{})}
}

// Get returns a deep copy of the current document.
func (s *Store) Get() topology.Document {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.doc.Clone()
}

// Replace normalizes and validates t, then stores it as the new desired state.
//
// The version advances only when the topology actually changed. A client that
// re-sends an identical document therefore does not trigger a reconcile, which
// keeps a chatty frontend from producing a reconcile storm. The bool result
// reports whether anything changed.
func (s *Store) Replace(t topology.Topology) (topology.Document, bool, error) {
	t = t.Clone()
	t.Normalize()
	if err := t.Validate(); err != nil {
		return topology.Document{}, false, fmt.Errorf("invalid topology: %w", err)
	}

	s.mu.Lock()
	if s.doc.Topology.Equal(t) {
		doc := s.doc.Clone()
		s.mu.Unlock()
		return doc, false, nil
	}
	s.doc.Topology = t
	s.doc.Version++
	doc := s.doc.Clone()
	s.mu.Unlock()

	s.notify()
	return doc, true, nil
}

// Subscribe returns a channel that receives a signal whenever the topology
// changes, and a function to unsubscribe.
//
// The channel is buffered with depth one and sends are non-blocking: a slow
// subscriber coalesces bursts into a single wake-up rather than applying
// pressure back onto the writer. That is exactly the semantics the reconciler
// wants — it always re-reads the latest desired state anyway.
func (s *Store) Subscribe() (<-chan struct{}, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextSub
	s.nextSub++
	ch := make(chan struct{}, 1)
	s.subs[id] = ch
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(c)
		}
	}
}

func (s *Store) notify() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// LayoutStore holds the client-owned layout document: canvas positions, zoom,
// cosmetics.
//
// It is intentionally an opaque JSON blob. The backend has no reason to
// understand layout, and giving it a schema would invite the two documents to
// grow into each other — the thing spec §4 explicitly forbids. Nothing here
// bumps the topology version or triggers a reconcile.
type LayoutStore struct {
	mu  sync.RWMutex
	raw json.RawMessage
}

// NewLayout returns a layout store holding an empty object.
func NewLayout() *LayoutStore {
	return &LayoutStore{raw: json.RawMessage(`{}`)}
}

// Get returns a copy of the current layout document.
func (l *LayoutStore) Get() json.RawMessage {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append(json.RawMessage(nil), l.raw...)
}

// Set replaces the layout document. It checks only that the payload is valid
// JSON.
func (l *LayoutStore) Set(raw json.RawMessage) error {
	if !json.Valid(raw) {
		return fmt.Errorf("layout: payload is not valid JSON")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.raw = append(json.RawMessage(nil), raw...)
	return nil
}
