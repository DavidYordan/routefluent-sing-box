package rfconnection

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/service"
)

type connectSelectionKey struct{}

// This is the selection actually held by the caller entering a dial, including
// a failed attempt. It is independent of live socket attribution and Now().
func WithConnectSelection(ctx context.Context, group, selected string) context.Context {
	if service.FromContext[*Observer](ctx) == nil {
		return ctx
	}
	previous, _ := ctx.Value(connectSelectionKey{}).([]string)
	chain := append(slices.Clone(previous), group, selected)
	return context.WithValue(ctx, connectSelectionKey{}, chain)
}

type ConnectOutcome struct {
	Outcome     string    `json:"outcome"`
	CompletedAt time.Time `json:"completed_at"`
	DurationMS  int64     `json:"duration_ms"`
	// Private native detail; public projections never copy destination errors.
	Error string `json:"error,omitempty"`
}

type ConnectResult struct {
	OutboundTag  string          `json:"outbound_tag"`
	Chain        []string        `json:"chain"`
	Succeeded    uint64          `json:"succeeded"`
	Failed       uint64          `json:"failed"`
	Canceled     uint64          `json:"canceled"`
	Latest       *ConnectOutcome `json:"latest,omitempty"`
	LastFailure  *ConnectOutcome `json:"last_failure,omitempty"`
	LastCanceled *ConnectOutcome `json:"last_canceled,omitempty"`
}

type ConnectResults struct {
	Schema  string          `json:"schema"`
	Epoch   string          `json:"epoch"`
	Entries []ConnectResult `json:"entries"`
}

// The SOCKS5 TCP client calls finish only after its real native dial returns.
// A nil result there follows a decoded successful CONNECT reply, not TCP alone.
// Counters have one key per observed configured selection/outbound, no flow IDs
// or destinations; they retire with this native instance and survive closes.
func StartSOCKS5Connect(ctx context.Context, tag string) func(error) {
	observer := service.FromContext[*Observer](ctx)
	if observer == nil {
		return nil
	}
	if _, dns := adapter.DNSTransportTagFromContext(ctx); dns {
		return nil
	}
	chain, _ := ctx.Value(connectSelectionKey{}).([]string)
	chain = append([]string{}, chain...)
	keyBytes, _ := json.Marshal(struct {
		Tag   string
		Chain []string
	}{tag, chain})
	key, started := string(keyBytes), time.Now()
	return func(err error) {
		result := ConnectOutcome{Outcome: "accepted", CompletedAt: time.Now().UTC(), DurationMS: time.Since(started).Milliseconds()}
		if err != nil {
			result.Outcome, result.Error = "failed", err.Error()
			if errors.Is(err, context.Canceled) {
				result.Outcome = "canceled"
			}
		}
		observer.mu.Lock()
		defer observer.mu.Unlock()
		entry := observer.connects[key]
		if entry == nil {
			entry = &ConnectResult{OutboundTag: tag, Chain: chain}
			observer.connects[key] = entry
		}
		switch result.Outcome {
		case "accepted":
			entry.Succeeded++
			entry.Latest = &result
		case "failed":
			entry.Failed++
			entry.Latest = &result
			entry.LastFailure = &result
		case "canceled":
			entry.Canceled++
			entry.LastCanceled = &result
		}
	}
}

func (o *Observer) ConnectSnapshot(epoch string) ConnectResults {
	o.mu.Lock()
	defer o.mu.Unlock()
	keys := make([]string, 0, len(o.connects))
	for key := range o.connects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := ConnectResults{Schema: "rf.native_connect_results.v1", Epoch: epoch, Entries: []ConnectResult{}}
	for _, key := range keys {
		entry := *o.connects[key]
		entry.Chain = slices.Clone(entry.Chain)
		if entry.Latest != nil {
			copy := *entry.Latest
			entry.Latest = &copy
		}
		if entry.LastFailure != nil {
			copy := *entry.LastFailure
			entry.LastFailure = &copy
		}
		if entry.LastCanceled != nil {
			copy := *entry.LastCanceled
			entry.LastCanceled = &copy
		}
		result.Entries = append(result.Entries, entry)
	}
	return result
}
