package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/bitcrafttech/hivenet/internal/driver/fake"
	"github.com/bitcrafttech/hivenet/internal/reconcile"
	"github.com/bitcrafttech/hivenet/internal/store"
	"github.com/bitcrafttech/hivenet/internal/topology"
)

const indexBody = "<!doctype html><title>Hivenet</title>"

func testServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	topologies := store.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := reconcile.NewEngine(reconcile.Options{
		Driver: fake.New(),
		Store:  topologies,
		Logger: logger,
	})
	srv := New(Options{
		Store:  topologies,
		Layout: store.NewLayout(),
		Engine: engine,
		Assets: fstest.MapFS{
			"index.html":    &fstest.MapFile{Data: []byte(indexBody)},
			"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
		},
		Logger: logger,
	})
	return srv, topologies
}

func do(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func validTopology() topology.Topology {
	return topology.Topology{Nodes: []topology.Node{{
		ID: "h1", Type: topology.TypeHost, Image: "alpine",
		Interfaces: []topology.Interface{{ID: "eth0", Name: "eth0"}},
	}}}
}

func TestGetTopologyStartsEmpty(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodGet, "/api/topology", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var doc topology.Document
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if doc.Version != 0 {
		t.Errorf("version = %d, want 0", doc.Version)
	}
}

func TestPutTopologyStoresAndVersions(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodPut, "/api/topology", topology.Document{Topology: validTopology()})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body)
	}
	var doc topology.Document
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if doc.Version != 1 {
		t.Errorf("version = %d, want 1", doc.Version)
	}
	if len(doc.Topology.Nodes) != 1 {
		t.Errorf("stored %d nodes, want 1", len(doc.Topology.Nodes))
	}
}

// The canvas is optimistic (spec §9), so two clients — or one client with a
// stale tab — can race. A stale version must be rejected rather than silently
// overwriting a change the sender never saw.
func TestPutTopologyRejectsStaleVersion(t *testing.T) {
	srv, _ := testServer(t)
	if rec := do(t, srv, http.MethodPut, "/api/topology", topology.Document{Topology: validTopology()}); rec.Code != http.StatusOK {
		t.Fatalf("setup PUT failed: %d %s", rec.Code, rec.Body)
	}

	stale := topology.Document{Topology: validTopology(), Version: 1}
	stale.Topology.Nodes[0].Image = "debian"
	if rec := do(t, srv, http.MethodPut, "/api/topology", stale); rec.Code != http.StatusOK {
		t.Fatalf("PUT at the current version should succeed, got %d: %s", rec.Code, rec.Body)
	}

	// Version is now 2; re-sending version 1 is stale.
	older := topology.Document{Topology: validTopology(), Version: 1}
	rec := do(t, srv, http.MethodPut, "/api/topology", older)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 for a stale version", rec.Code)
	}
}

func TestPutTopologyReturnsPerFieldValidationErrors(t *testing.T) {
	srv, _ := testServer(t)
	broken := topology.Topology{Nodes: []topology.Node{{ID: "h1", Type: "nonsense"}}}

	rec := do(t, srv, http.MethodPut, "/api/topology", topology.Document{Topology: broken})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422. body: %s", rec.Code, rec.Body)
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Details) == 0 {
		t.Fatal("expected per-field details so the UI can mark the offending field")
	}
	for _, d := range body.Details {
		if d.Path == "" {
			t.Errorf("validation detail is missing a field path: %+v", d)
		}
	}
}

func TestPutTopologyRejectsMalformedJSON(t *testing.T) {
	srv, _ := testServer(t)
	req := httptest.NewRequest(http.MethodPut, "/api/topology", bytes.NewReader([]byte("{oops")))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// Spec §4: the layout document is client-owned and must never touch the
// authoritative topology or its version.
func TestLayoutIsIsolatedFromTopology(t *testing.T) {
	srv, topologies := testServer(t)
	if rec := do(t, srv, http.MethodPut, "/api/topology", topology.Document{Topology: validTopology()}); rec.Code != http.StatusOK {
		t.Fatalf("setup PUT failed: %d %s", rec.Code, rec.Body)
	}
	versionBefore := topologies.Get().Version

	layout := map[string]any{"nodes": map[string]any{"h1": map[string]any{"x": 12, "y": 34}}}
	if rec := do(t, srv, http.MethodPut, "/api/layout", layout); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT layout status = %d, want 204. body: %s", rec.Code, rec.Body)
	}

	if got := topologies.Get().Version; got != versionBefore {
		t.Errorf("a layout write bumped the topology version from %d to %d", versionBefore, got)
	}

	rec := do(t, srv, http.MethodGet, "/api/layout", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET layout status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode layout: %v", err)
	}
	if _, ok := got["nodes"]; !ok {
		t.Errorf("layout round trip lost data: %v", got)
	}
}

func TestStatusEndpoint(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodGet, "/api/status", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var status reconcile.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
}

func TestReconcileEndpointIsAsynchronous(t *testing.T) {
	srv, _ := testServer(t)
	if rec := do(t, srv, http.MethodPost, "/api/reconcile", nil); rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", rec.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodDelete, "/api/topology", nil)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") == "" {
		t.Error("405 response is missing an Allow header")
	}
}

func TestStaticAssetsAreServed(t *testing.T) {
	srv, _ := testServer(t)

	if rec := do(t, srv, http.MethodGet, "/", nil); rec.Body.String() != indexBody {
		t.Errorf("GET / served %q, want the index shell", rec.Body.String())
	}
	if rec := do(t, srv, http.MethodGet, "/assets/app.js", nil); rec.Code != http.StatusOK {
		t.Errorf("GET /assets/app.js status = %d, want 200", rec.Code)
	}
}

// A hard refresh on a client-side route must render the app shell, not a 404.
func TestUnknownPathFallsBackToIndex(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodGet, "/topology/some-client-route", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != indexBody {
		t.Errorf("served %q, want the index shell", rec.Body.String())
	}
}

func TestTerminalEndpointReportsNotImplemented(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodGet, "/ws/terminal?node=h1", nil)

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 while the terminal is unbuilt", rec.Code)
	}
}

func TestHealthz(t *testing.T) {
	srv, _ := testServer(t)
	if rec := do(t, srv, http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
