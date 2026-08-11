package reconcile

import (
	"context"
	"time"
)

// TrafficInterval is how often link byte counters are sampled and diffed
// into deltas for the traffic_stats WebSocket push (HIVENET_MVP_SPEC.md
// section 11 step 7 wants ~1Hz).
const TrafficInterval = time.Second

// TrafficDirection names which way a byte delta travelled across a link,
// relative to the order its two ends appear in topology.Link.Endpoints.
// The frontend already has that ordering from the last topology_status
// push, so a short tag is enough to point the moving-dot animation the
// right way.
type TrafficDirection string

const (
	// DirForward is Endpoints[0] -> Endpoints[1] traffic.
	DirForward TrafficDirection = "fwd"
	// DirReverse is Endpoints[1] -> Endpoints[0] traffic.
	DirReverse TrafficDirection = "rev"
)

// TrafficDelta is one direction's byte count observed for one link since
// the previous poll tick. It is the payload of a traffic_stats WebSocket
// push; a tick that sees no traffic on a link in a direction omits that
// direction's sample rather than sending a wasted zero.
type TrafficDelta struct {
	LinkID     string           `json:"linkId"`
	Direction  TrafficDirection `json:"direction"`
	BytesDelta uint64           `json:"bytesDelta"`
}

// SubscribeTraffic returns a channel of TrafficDelta batches and an
// unsubscribe function, mirroring Subscribe's contract: the channel holds
// only the newest batch, and a slow consumer drops frames rather than
// stalling the poller.
func (e *Engine) SubscribeTraffic() (<-chan []TrafficDelta, func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := e.nextTrafficSub
	e.nextTrafficSub++
	ch := make(chan []TrafficDelta, 1)
	e.trafficSubs[id] = ch
	return ch, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if c, ok := e.trafficSubs[id]; ok {
			delete(e.trafficSubs, id)
			close(c)
		}
	}
}

// pollTraffic samples every link the last reconcile cycle reported as up,
// diffs each against its previous reading, and publishes the non-zero
// deltas as one batch. It runs on the Run goroutine, the same one that
// makes every other podman/netlink call, so it never races a concurrent
// CreateLink/DestroyLink touching the same veth.
func (e *Engine) pollTraffic(ctx context.Context) {
	st := e.Status()
	doc := e.store.Get()

	live := make(map[string]struct{}, len(doc.Topology.Links))
	var samples []TrafficDelta

	for _, link := range doc.Topology.Links {
		live[link.ID] = struct{}{}

		if st.Links[link.ID].State != StateUp {
			// Not actually wired yet (or drifted away) -- the veth this
			// would read may not exist, and even if it does, a link that
			// is not up is not one the animation should show traffic on.
			continue
		}

		counters, err := e.drv.LinkCounters(ctx, e.owner, link)
		if err != nil {
			e.log.Debug("traffic poll failed", "link", link.ID, "error", err)
			continue
		}

		prev, seen := e.trafficBaseline[link.ID]
		e.trafficBaseline[link.ID] = counters
		if !seen {
			// First sighting only establishes a baseline. Emitting a
			// delta here would either be the link's entire lifetime
			// traffic as one spike, or garbage if the counters did not
			// start at zero (e.g. a node recreated without its link).
			continue
		}

		if d := counterDelta(prev.TxBytes, counters.TxBytes); d > 0 {
			samples = append(samples, TrafficDelta{LinkID: link.ID, Direction: DirForward, BytesDelta: d})
		}
		if d := counterDelta(prev.RxBytes, counters.RxBytes); d > 0 {
			samples = append(samples, TrafficDelta{LinkID: link.ID, Direction: DirReverse, BytesDelta: d})
		}
	}

	// Drop baselines for links that no longer exist, so if the same link
	// ID ever reappears (recreated, counters reset to zero by the fresh
	// veth) it is treated as a first sighting again rather than reading
	// as a huge negative delta.
	for id := range e.trafficBaseline {
		if _, ok := live[id]; !ok {
			delete(e.trafficBaseline, id)
		}
	}

	e.publishTraffic(samples)
}

// counterDelta diffs two cumulative kernel counters. A counter that went
// backwards means the interface was recreated since the last reading (a
// link destroyed and rebuilt under the same ID within one poll interval);
// the new value is used as-is rather than underflowing.
func counterDelta(prev, cur uint64) uint64 {
	if cur < prev {
		return cur
	}
	return cur - prev
}

// publishTraffic fans a batch out to every traffic subscriber, keeping
// only the newest batch in a slow consumer's slot -- the same
// drop-intermediate-frames policy setStatus uses for Status.
func (e *Engine) publishTraffic(samples []TrafficDelta) {
	if len(samples) == 0 {
		return
	}

	e.mu.RLock()
	subs := make([]chan []TrafficDelta, 0, len(e.trafficSubs))
	for _, ch := range e.trafficSubs {
		subs = append(subs, ch)
	}
	e.mu.RUnlock()

	for _, ch := range subs {
		select {
		case ch <- samples:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- samples:
			default:
			}
		}
	}
}
