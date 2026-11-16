package trafficcontrol

// The epoch belongs to the same manager that supplies Total. A new native
// instance, including a reload in the same PID, owns different counters.
// Entropy failure rejects only this optional observation, never native Start.
func (m *Manager) RFTrafficEpoch() (string, error) {
	return m.rfTrafficEpochID, m.rfTrafficEpochError
}
