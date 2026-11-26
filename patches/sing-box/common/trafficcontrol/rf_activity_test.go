package trafficcontrol

import (
	"encoding/json"
	"github.com/gofrs/uuid/v5"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type rfActivityTracker struct{ value TrackerMetadata }

func (t *rfActivityTracker) Metadata() *TrackerMetadata { return &t.value }
func (t *rfActivityTracker) Close() error               { return nil }

func TestRFSelectedLeavesUnobservedSocketsWithNativeOwner(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	if got := RFSelectedConnection(t.Context(), left, "group", "selected"); got != left {
		t.Fatal("an unobserved native instance acquired an observation wrapper")
	}
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer packet.Close()
	if got := RFSelectedPacketConnection(t.Context(), packet, "group", "selected"); got != packet {
		t.Fatal("an unobserved native packet socket acquired an observation wrapper")
	}
}

func TestRFUsageIncludesClosedEvictedAndClearedConnectionsOnce(t *testing.T) {
	m := NewManager(nil)
	chain := []string{"row-selector", "scheme-x"}
	for index := 0; index < closedConnectionsLimit+17; index++ {
		tracker := &rfActivityTracker{value: TrackerMetadata{ID: uuid.Must(uuid.NewV4()), rfSelection: &rfSelection{}, Upload: new(atomic.Int64), Download: new(atomic.Int64)}}
		tracker.value.rfSelection.store(chain)
		tracker.value.Upload.Store(10)
		tracker.value.Download.Store(20)
		m.join(tracker)
		m.leave(tracker)
		m.leave(tracker) // Native close may be invoked twice.
	}
	check := func() RFActivity {
		t.Helper()
		value, err := m.RFActivitySnapshot()
		if err != nil || len(value.Flows) != 0 || value.Usage == nil || len(value.Usage.Totals) != 1 || value.Usage.Totals[0].Upload != int64((closedConnectionsLimit+17)*10) || value.Usage.Totals[0].Download != int64((closedConnectionsLimit+17)*20) || value.Usage.StartedAt.IsZero() {
			t.Fatalf("completed connections lost or duplicated: %#v, %v", value, err)
		}
		return value
	}
	value := check()
	value.Usage.Totals[0].Chain[0] = "mutated-observation"
	m.Clear()
	m.Clear()
	if got := check(); got.Usage.Totals[0].Chain[0] != chain[0] {
		t.Fatal("public capture changed the manager's aggregate")
	}
	if next := NewManager(nil); next.rfTrafficEpochID == m.rfTrafficEpochID || next.rfTrafficStartedAt.Before(m.rfTrafficStartedAt) || time.Since(next.rfTrafficStartedAt) > time.Minute {
		t.Fatal("new runtime reused a traffic baseline")
	}
}

func TestRFUsageDoesNotRetroactivelyAttributeUnboundBytes(t *testing.T) {
	m := NewManager(nil)
	upload, download := new(atomic.Int64), new(atomic.Int64)
	upload.Store(100)
	download.Store(200)
	tracker := &rfActivityTracker{value: TrackerMetadata{ID: uuid.Must(uuid.NewV4()), rfSelection: &rfSelection{upload: upload, download: download}, Upload: upload, Download: download}}
	m.join(tracker)
	tracker.value.rfSelection.store([]string{"row-selector", "scheme-y"})
	upload.Add(11)
	download.Add(22)
	m.leave(tracker)
	m.Clear()
	value, err := m.RFActivitySnapshot()
	if err != nil || len(value.Usage.Totals) != 2 {
		t.Fatal(value, err)
	}
	for _, total := range value.Usage.Totals {
		if len(total.Chain) == 0 {
			if total.Upload != 100 || total.Download != 200 {
				t.Fatal(total)
			}
		} else if total.Upload != 11 || total.Download != 22 {
			t.Fatal(total)
		}
	}
}

func TestRFActivityDoesNotClaimBeforeDialSelection(t *testing.T) {
	m := NewManager(nil)
	tracker := &rfActivityTracker{value: TrackerMetadata{ID: uuid.Must(uuid.NewV4()), Chain: []string{"route-selector", "selection-before-dial"}, Upload: new(atomic.Int64), Download: new(atomic.Int64)}}
	tracker.value.Upload.Store(100)
	tracker.value.Download.Store(200)
	m.connections.Store(tracker.value.ID, tracker)
	observed, err := m.RFActivitySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.Flows) != 1 || len(observed.Flows[0].Chain) != 0 || observed.Flows[0].Upload != 100 || observed.Flows[0].Download != 200 {
		t.Fatalf("before-dial selection became actual transport attribution: %#v", observed)
	}
}

func TestRFActivityKeepsLiveManagerIdentityAndNoDestinations(t *testing.T) {
	m := NewManager(nil)
	tracker := &rfActivityTracker{value: TrackerMetadata{ID: uuid.Must(uuid.NewV4()), Chain: []string{"route-selector", "selected-target"}, Upload: new(atomic.Int64), Download: new(atomic.Int64)}}
	tracker.value.rfSelection = &rfSelection{}
	tracker.value.rfSelection.store([]string{"route-selector", "selected-target"})
	tracker.value.Metadata.Domain = "private-customer-destination.invalid"
	tracker.value.Upload.Store(100)
	tracker.value.Download.Store(200)
	m.connections.Store(tracker.value.ID, tracker)
	first, err := m.RFActivitySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Flows) != 1 || first.Flows[0].Upload != 100 || first.Flows[0].Download != 200 {
		t.Fatal(first)
	}
	bytes, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bytes), "private-customer") || strings.Contains(string(bytes), "metadata") {
		t.Fatal("private metadata escaped", string(bytes))
	}
	first.Flows[0].Chain[0] = "mutated-result"
	tracker.value.Upload.Add(10)
	second, err := m.RFActivitySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if second.Epoch != first.Epoch || second.Flows[0].Chain[0] != "route-selector" || second.Flows[0].Upload != 110 {
		t.Fatal("observation mutated native counters or chain", second)
	}
	m.connections.Delete(tracker.value.ID)
	closed, err := m.RFActivitySnapshot()
	if err != nil || len(closed.Flows) != 0 {
		t.Fatal("closed activity retained history", closed, err)
	}
	reloaded, err := NewManager(nil).RFActivitySnapshot()
	if err != nil || reloaded.Epoch == first.Epoch {
		t.Fatal("new manager reused old baseline", reloaded, err)
	}
}
