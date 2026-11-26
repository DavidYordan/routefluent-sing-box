package trafficcontrol

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	C "github.com/sagernet/sing-box/constant"
)

// RFActivity contains no client, destination, rule or application metadata.
// The same manager owns its epoch and currently live transport counters.
type RFActivity struct {
	Schema string           `json:"schema"`
	Epoch  string           `json:"epoch"`
	Flows  []RFActivityFlow `json:"flows"`
	Usage  *RFActivityUsage `json:"usage,omitempty"`
}

// The manager's existing closed/live accounting owns these payload totals.
// Only evicted totals survive as one aggregate per actual selected chain.
type RFActivityUsage struct {
	StartedAt time.Time         `json:"started_at"`
	Totals    []RFActivityTotal `json:"totals"`
}

type RFActivityTotal struct {
	Chain    []string `json:"chain"`
	Upload   int64    `json:"upload"`
	Download int64    `json:"download"`
}

func rfAddTraffic(totals map[string]RFActivityTotal, metadata *TrackerMetadata, upload, download int64) {
	if metadata.OutboundType == C.TypeDNS {
		return
	}
	chain, unboundUpload, unboundDownload := metadata.rfSelection.usageScope(upload, download)
	add := func(chain []string, upload, download int64) {
		key, _ := json.Marshal(chain)
		value := totals[string(key)]
		value.Chain = chain
		value.Upload += upload
		value.Download += download
		totals[string(key)] = value
	}
	add(chain, upload-unboundUpload, download-unboundDownload)
	if unboundUpload != 0 || unboundDownload != 0 {
		add([]string{}, unboundUpload, unboundDownload)
	}
}

// Called only while the native closed-connections owner holds its mutex.
func (m *Manager) rfRetireTraffic(metadata *TrackerMetadata, upload, download int64) {
	if m.rfClosedTraffic == nil {
		m.rfClosedTraffic = make(map[string]RFActivityTotal)
	}
	rfAddTraffic(m.rfClosedTraffic, metadata, upload, download)
}

type RFActivityFlow struct {
	ID       string   `json:"id"`
	Chain    []string `json:"chain"`
	Upload   int64    `json:"upload"`
	Download int64    `json:"download"`
}

func (m *Manager) RFActivitySnapshot() (RFActivity, error) {
	epoch, err := m.RFTrafficEpoch()
	if err != nil {
		return RFActivity{}, err
	}
	result := RFActivity{Schema: "rf.native_activity.v1", Epoch: epoch, Flows: []RFActivityFlow{}}
	m.closedConnectionsAccess.Lock()
	defer m.closedConnectionsAccess.Unlock()
	totals := make(map[string]RFActivityTotal, len(m.rfClosedTraffic))
	for key, value := range m.rfClosedTraffic {
		totals[key] = value
	}
	for element := m.closedConnections.Front(); element != nil; element = element.Next() {
		metadata := element.Value
		rfAddTraffic(totals, &metadata, metadata.Upload.Load(), metadata.Download.Load())
	}
	for _, flow := range m.Connections() {
		if flow.OutboundType == C.TypeDNS {
			continue
		}
		upload, download := flow.Upload.Load(), flow.Download.Load()
		result.Flows = append(result.Flows, RFActivityFlow{ID: flow.ID.String(), Chain: flow.rfSelection.snapshot(), Upload: upload, Download: download})
		rfAddTraffic(totals, flow, upload, download)
	}
	result.Usage = &RFActivityUsage{StartedAt: m.rfTrafficStartedAt, Totals: []RFActivityTotal{}}
	for _, value := range totals {
		value.Chain = slices.Clone(value.Chain)
		result.Usage.Totals = append(result.Usage.Totals, value)
	}
	slices.SortFunc(result.Usage.Totals, func(a, b RFActivityTotal) int {
		left, _ := json.Marshal(a.Chain)
		right, _ := json.Marshal(b.Chain)
		return strings.Compare(string(left), string(right))
	})
	return result, nil
}
