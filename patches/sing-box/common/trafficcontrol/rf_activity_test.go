package trafficcontrol

import (
	"encoding/json"
	"github.com/gofrs/uuid/v5"
	"strings"
	"sync/atomic"
	"testing"
)

type rfActivityTracker struct{ value TrackerMetadata }

func (t *rfActivityTracker) Metadata() *TrackerMetadata { return &t.value }
func (t *rfActivityTracker) Close() error               { return nil }

func TestRFActivityKeepsLiveManagerIdentityAndNoDestinations(t *testing.T) {
	m := NewManager(nil)
	tracker := &rfActivityTracker{value: TrackerMetadata{ID: uuid.Must(uuid.NewV4()), Chain: []string{"route-selector", "selected-target"}, Upload: new(atomic.Int64), Download: new(atomic.Int64)}}
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
