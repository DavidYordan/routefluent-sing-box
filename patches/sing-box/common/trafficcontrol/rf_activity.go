package trafficcontrol

import C "github.com/sagernet/sing-box/constant"

// RFActivity contains no client, destination, rule or application metadata.
// The same manager owns its epoch and currently live transport counters.
type RFActivity struct {
	Schema string           `json:"schema"`
	Epoch  string           `json:"epoch"`
	Flows  []RFActivityFlow `json:"flows"`
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
	for _, flow := range m.Connections() {
		if flow.OutboundType == C.TypeDNS {
			continue
		}
		result.Flows = append(result.Flows, RFActivityFlow{ID: flow.ID.String(), Chain: flow.rfSelection.snapshot(), Upload: flow.Upload.Load(), Download: flow.Download.Load()})
	}
	return result, nil
}
