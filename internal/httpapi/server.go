// Package httpapi serves the REST surface, the WebSocket endpoints and the
// embedded frontend from a single HTTP server (spec §2).
//
// Handlers never call the driver. They mutate the store and return; the
// reconciler converges asynchronously and reports back over /ws. This is what
// keeps the reconciler the single writer.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/bitcrafttech/hivenet/internal/auth"
	"github.com/bitcrafttech/hivenet/internal/reconcile"
	"github.com/bitcrafttech/hivenet/internal/store"
	"github.com/bitcrafttech/hivenet/internal/topology"
)

// maxBodyBytes caps request bodies. Topologies are small documents; anything
// larger is a mistake or an attack.
const maxBodyBytes = 4 << 20 // 4 MiB

// Server wires the stores, the reconcile engine and the embedded assets into an
// http.Handler.
type Server struct {
	store  *store.Store
	layout *store.LayoutStore
	engine *reconcile.Engine
	assets fs.FS
	log    *slog.Logger

	// originPatterns is the WebSocket origin allowlist. Empty means same-origin
	// only, which is correct for the single-binary deployment.
	originPatterns []string

	mux *http.ServeMux
}

// Options configures a Server. Store, Layout and Engine are required.
type Options struct {
	Store          *store.Store
	Layout         *store.LayoutStore
	Engine         *reconcile.Engine
	Assets         fs.FS
	Auth           auth.Authenticator
	Logger         *slog.Logger
	OriginPatterns []string
}

// New builds the HTTP handler.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Server{
		store:          opts.Store,
		layout:         opts.Layout,
		engine:         opts.Engine,
		assets:         opts.Assets,
		log:            opts.Logger,
		originPatterns: opts.OriginPatterns,
		mux:            http.NewServeMux(),
	}

	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/api/topology", s.handleTopology)
	s.mux.HandleFunc("/api/layout", s.handleLayout)
	s.mux.HandleFunc("/api/status", s.handleStatus)
	s.mux.HandleFunc("/api/reconcile", s.handleReconcile)
	s.mux.HandleFunc("/ws", s.handleWS)
	s.mux.HandleFunc("/ws/terminal", s.handleTerminal)
	s.mux.Handle("/", s.staticHandler())

	return s
}

// Handler returns the root handler with the auth middleware applied.
func (s *Server) Handler(a auth.Authenticator) http.Handler {
	return auth.Middleware(a)(s.mux)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth.Middleware(auth.NoOp{})(s.mux).ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleTopology reads and replaces the authoritative topology document.
//
// PUT accepts a Document. If its version is non-zero it is treated as an
// optimistic-concurrency check: a mismatch returns 409 rather than silently
// overwriting a change the client has not seen yet.
func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.store.Get())

	case http.MethodPut, http.MethodPost:
		var incoming topology.Document
		if err := decodeJSON(w, r, &incoming); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), nil)
			return
		}
		if current := s.store.Get(); incoming.Version != 0 && incoming.Version != current.Version {
			writeError(w, http.StatusConflict, fmt.Sprintf(
				"topology has changed: you sent version %d, current is %d",
				incoming.Version, current.Version), nil)
			return
		}

		doc, changed, err := s.store.Replace(incoming.Topology)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "invalid topology", validationDetails(err))
			return
		}
		// The store notifies the engine on a real change; this only covers the
		// case where a client wants a cycle even though nothing differed.
		if !changed {
			s.log.Debug("topology unchanged, no reconcile triggered", "version", doc.Version)
		}
		writeJSON(w, http.StatusOK, doc)

	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodPost)
	}
}

// handleLayout reads and replaces the client-owned layout document. It never
// touches the topology version and never triggers a reconcile (spec §4).
func (s *Server) handleLayout(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(s.layout.Get())

	case http.MethodPut, http.MethodPost:
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeError(w, http.StatusBadRequest, "could not read body", nil)
			return
		}
		if err := s.layout.Set(body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), nil)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodPost)
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, s.engine.Status())
}

// handleReconcile requests a cycle explicitly. It returns immediately — the
// result arrives over /ws like any other cycle.
func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	s.engine.Trigger()
	w.WriteHeader(http.StatusAccepted)
}

// staticHandler serves the embedded frontend, falling back to index.html for
// unknown paths so client-side routing works on a hard refresh.
func (s *Server) staticHandler() http.Handler {
	if s.assets == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "frontend assets not built", http.StatusNotFound)
		})
	}
	files := http.FileServer(http.FS(s.assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" || name == "." {
			name = "index.html"
		}
		if _, err := fs.Stat(s.assets, name); err != nil {
			s.serveIndex(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	body, err := fs.ReadFile(s.assets, "index.html")
	if err != nil {
		http.Error(w, "frontend assets not built", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("could not parse JSON body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The header is already written, so there is nothing to do but note it.
		slog.Default().Error("write json response", "error", err)
	}
}

type errorBody struct {
	Error   string                     `json:"error"`
	Details []topology.ValidationError `json:"details,omitempty"`
}

func writeError(w http.ResponseWriter, code int, msg string, details []topology.ValidationError) {
	writeJSON(w, code, errorBody{Error: msg, Details: details})
}

// validationDetails unpacks the joined error from topology.Validate back into
// per-field entries, so the frontend can attach each message to the field that
// caused it instead of showing one long string.
func validationDetails(err error) []topology.ValidationError {
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		var one topology.ValidationError
		if errors.As(err, &one) {
			return []topology.ValidationError{one}
		}
		return nil
	}
	var out []topology.ValidationError
	for _, e := range joined.Unwrap() {
		var ve topology.ValidationError
		if errors.As(e, &ve) {
			out = append(out, ve)
		} else {
			out = append(out, topology.ValidationError{Msg: e.Error()})
		}
	}
	return out
}

func methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
}
