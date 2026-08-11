package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/bitcrafttech/hivenet/internal/driver"
)

// TestPollTrafficEmitsDeltasAfterBaseline checks the two-tick contract: the
// first poll after a link comes up only records a baseline (no publish,
// nothing would be diffable yet), and the next poll reports exactly the
// counters advanced since that baseline, split by direction.
func TestPollTrafficEmitsDeltasAfterBaseline(t *testing.T) {
	eng, drv, st := testEngine(t, 0)
	mustReplace(t, st, pair())

	status := eng.ReconcileNow(context.Background())
	if got := status.Links["l1"].State; got != StateUp {
		t.Fatalf("link l1 state = %q, want up (%s)", got, status.Links["l1"].Message)
	}

	sub, unsub := eng.SubscribeTraffic()
	defer unsub()

	drv.SetLinkCounters("l1", driver.LinkCounters{TxBytes: 1000, RxBytes: 500})
	eng.pollTraffic(context.Background())
	select {
	case got := <-sub:
		t.Fatalf("first poll after baseline published %v, want nothing", got)
	default:
	}

	drv.SetLinkCounters("l1", driver.LinkCounters{TxBytes: 1500, RxBytes: 700})
	eng.pollTraffic(context.Background())
	select {
	case got := <-sub:
		want := []TrafficDelta{
			{LinkID: "l1", Direction: DirForward, BytesDelta: 500},
			{LinkID: "l1", Direction: DirReverse, BytesDelta: 200},
		}
		if len(got) != len(want) {
			t.Fatalf("samples = %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("sample %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	default:
		t.Fatal("second poll published nothing, want a forward and reverse sample")
	}
}

// TestPollTrafficSkipsLinksNotUp checks that a link the last reconcile did
// not report as up is never probed: pollTraffic must not publish, and must
// not panic, when Status has no entry for the link at all (nothing has
// reconciled yet).
func TestPollTrafficSkipsLinksNotUp(t *testing.T) {
	eng, drv, st := testEngine(t, 0)
	mustReplace(t, st, pair())

	sub, unsub := eng.SubscribeTraffic()
	defer unsub()

	drv.SetLinkCounters("l1", driver.LinkCounters{TxBytes: 999, RxBytes: 999})
	eng.pollTraffic(context.Background())
	select {
	case got := <-sub:
		t.Fatalf("polling a link that never reconciled published %v, want nothing", got)
	default:
	}
}

// TestPollTrafficSkipsOnDriverError checks that a failing LinkCounters call
// is logged and skipped rather than stopping the whole poll tick or
// panicking.
func TestPollTrafficSkipsOnDriverError(t *testing.T) {
	eng, drv, st := testEngine(t, 0)
	mustReplace(t, st, pair())

	status := eng.ReconcileNow(context.Background())
	if got := status.Links["l1"].State; got != StateUp {
		t.Fatalf("link l1 state = %q, want up (%s)", got, status.Links["l1"].Message)
	}

	sub, unsub := eng.SubscribeTraffic()
	defer unsub()

	drv.FailOp("LinkCounters:l1", errors.New("simulated netlink failure"))
	eng.pollTraffic(context.Background())
	select {
	case got := <-sub:
		t.Fatalf("poll with a failing LinkCounters call published %v, want nothing", got)
	default:
	}
}

// TestPollTrafficDropsBaselineOnLinkRemoval checks that a link's baseline
// is forgotten once it disappears from the desired topology, so if the
// same link ID is ever reused it is treated as a first sighting again
// instead of reading as a huge negative delta against a stale reading.
func TestPollTrafficDropsBaselineOnLinkRemoval(t *testing.T) {
	eng, drv, st := testEngine(t, 0)
	mustReplace(t, st, pair())
	eng.ReconcileNow(context.Background())

	drv.SetLinkCounters("l1", driver.LinkCounters{TxBytes: 1000, RxBytes: 500})
	eng.pollTraffic(context.Background())
	if _, ok := eng.trafficBaseline["l1"]; !ok {
		t.Fatal("baseline not recorded after first poll")
	}

	nodesOnly := pair()
	nodesOnly.Links = nil
	mustReplace(t, st, nodesOnly)
	eng.pollTraffic(context.Background())
	if _, ok := eng.trafficBaseline["l1"]; ok {
		t.Fatal("baseline for a removed link was not dropped")
	}
}
