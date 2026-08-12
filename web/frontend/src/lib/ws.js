// Client for the single WebSocket documented in internal/httpapi/ws.go.
// Two message kinds share one connection ("topology_status" and
// "traffic_stats") so a status re-render never blocks the ~1Hz traffic
// tick; callers register a handler per kind.

const RECONNECT_DELAY_MS = 1500;

export function connectTopologySocket({ onTopologyStatus, onTrafficStats, onOpen, onClose }) {
  let closed = false;
  let socket;

  function connect() {
    if (closed) return;
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    socket = new WebSocket(`${proto}//${location.host}/ws`);

    socket.onopen = () => onOpen?.();
    socket.onmessage = (ev) => {
      let msg;
      try {
        msg = JSON.parse(ev.data);
      } catch {
        return;
      }
      if (msg.type === 'topology_status') onTopologyStatus?.(msg.data);
      else if (msg.type === 'traffic_stats') onTrafficStats?.(msg.data);
    };
    socket.onclose = () => {
      onClose?.();
      if (!closed) setTimeout(connect, RECONNECT_DELAY_MS);
    };
    socket.onerror = () => socket.close();
  }

  connect();

  return {
    close() {
      closed = true;
      socket?.close();
    },
  };
}
