package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// Message types carried over the topology WebSocket (spec §9).
//
// They are deliberately distinct types on one connection so that a status
// re-render never blocks the ~1 Hz traffic animation tick. msgTrafficStats has
// no producer yet — traffic polling is build order step 7.
const (
	msgTopologyStatus = "topology_status"
	msgTrafficStats   = "traffic_stats"
)

// envelope is the wire format for every message on the topology socket.
type envelope struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// wsWriteTimeout bounds a single frame write, so one wedged client cannot hold
// a goroutine forever.
const wsWriteTimeout = 10 * time.Second

// handleWS streams reconcile results to the canvas.
//
// The terminal deliberately does not share this connection: a laggy shell
// session must not be able to back up the topology and traffic streams
// (spec §9).
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.originPatterns,
	})
	if err != nil {
		s.log.Warn("websocket accept failed", "error", err)
		return
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	updates, unsubscribe := s.engine.Subscribe()
	defer unsubscribe()

	// The client sends nothing on this socket. Reading anyway is how a
	// disconnect is noticed promptly, and it keeps control frames drained.
	go func() {
		defer cancel()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()

	// Send the current picture immediately so a page load does not sit blank
	// until the next reconcile.
	if err := writeMessage(ctx, conn, msgTopologyStatus, s.engine.Status()); err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case status, ok := <-updates:
			if !ok {
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			if err := writeMessage(ctx, conn, msgTopologyStatus, status); err != nil {
				s.log.Debug("websocket write failed, dropping client", "error", err)
				return
			}
		}
	}
}

func writeMessage(ctx context.Context, conn *websocket.Conn, kind string, data any) error {
	payload, err := json.Marshal(envelope{Type: kind, Data: data})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, payload)
}

// handleTerminal is build order step 10: a pty per node over its own
// WebSocket, straight to `podman exec`. Stubbed so the route exists and the
// frontend gets an honest error instead of a 404 it has to interpret.
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "node terminal is not implemented yet (build order step 10)", http.StatusNotImplemented)
}
