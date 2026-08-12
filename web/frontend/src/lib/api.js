// Thin wrappers around the REST surface documented in internal/httpapi/server.go.
// Every write is a full-document PUT with optimistic concurrency (spec section 4):
// the server rejects a stale `version` with 409 rather than silently
// overwriting a change the client hasn't seen yet.

async function asJSON(res) {
  if (!res.ok) {
    let detail = '';
    try {
      detail = await res.text();
    } catch {
      // ignore
    }
    throw new Error(`${res.status} ${res.statusText}${detail ? ': ' + detail : ''}`);
  }
  if (res.status === 204) return null;
  return res.json();
}

/**
 * The backend marshals a nil Go slice as JSON `null`, so an empty node or
 * link list round-trips as `null` rather than `[]`. Every caller of
 * getTopology/putTopology expects arrays it can safely call .length/.map on,
 * so we normalize here once instead of relying on every consumer to guard
 * with `?? []`.
 */
function normalizeTopologyDoc(doc) {
  return {
    version: doc?.version ?? 0,
    topology: {
      nodes: doc?.topology?.nodes ?? [],
      links: doc?.topology?.links ?? [],
    },
  };
}

/** GET /api/topology -> Document{topology,version} */
export async function getTopology() {
  return normalizeTopologyDoc(await asJSON(await fetch('/api/topology')));
}

/**
 * PUT /api/topology with the full desired document. Throws on 409
 * (version conflict) or 422 (validation) so callers can decide how to
 * recover -- v1 just surfaces the error, it never auto-merges.
 */
export async function putTopology(topology, version) {
  return normalizeTopologyDoc(
    await asJSON(
      await fetch('/api/topology', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ topology, version }),
      }),
    ),
  );
}

/** GET /api/layout -> opaque client-owned JSON blob, {} if never saved. */
export async function getLayout() {
  const res = await fetch('/api/layout');
  if (!res.ok) return {};
  const text = await res.text();
  if (!text) return {};
  try {
    return JSON.parse(text);
  } catch {
    return {};
  }
}

/** PUT /api/layout with an opaque blob -- the server never inspects it. */
export async function putLayout(layout) {
  await fetch('/api/layout', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(layout),
  });
}

/** GET /api/status -> reconcile.Status snapshot (used as a WS-less fallback). */
export async function getStatus() {
  return asJSON(await fetch('/api/status'));
}

/** POST /api/reconcile -> ask for a cycle now instead of waiting on debounce. */
export async function triggerReconcile() {
  await fetch('/api/reconcile', { method: 'POST' });
}
