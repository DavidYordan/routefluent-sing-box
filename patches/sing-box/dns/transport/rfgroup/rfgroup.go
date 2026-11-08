package rfgroup

import (
	"context"
	"errors"
	"fmt"

	mDNS "github.com/miekg/dns"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	boxdns "github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/service"
)

var _ adapter.DNSTransport = (*Transport)(nil)
var _ adapter.DNSTransportWithEnvironment = (*Transport)(nil)

func RegisterTransport(registry *boxdns.TransportRegistry) {
	boxdns.RegisterTransport[option.RFResolverGroupDNSServerOptions](registry, C.DNSTypeRFResolverGroup, NewTransport)
}

type Transport struct {
	boxdns.TransportAdapter
	logger      log.ContextLogger
	manager     adapter.DNSTransportManager
	primaryTags []string
	// Dependencies resolve once at native startup, before requests can enter.
	// Outcomes never change this selected policy or its cache identity.
	primaries []adapter.DNSTransport
}

func NewTransport(ctx context.Context, logger log.ContextLogger, tag string, options option.RFResolverGroupDNSServerOptions) (adapter.DNSTransport, error) {
	if tag == "" || len(options.Primary) == 0 {
		return nil, E.New("rf resolver group requires a tag and at least one primary DoH server")
	}
	seen := make(map[string]bool, len(options.Primary))
	for _, primary := range options.Primary {
		if primary == "" || primary == tag || seen[primary] {
			return nil, E.New("rf resolver group primary tag is empty, repeated or references itself")
		}
		seen[primary] = true
	}
	manager := service.FromContext[adapter.DNSTransportManager](ctx)
	if manager == nil {
		return nil, E.New("missing DNS transport manager in context")
	}
	tags := append([]string(nil), options.Primary...)
	return &Transport{
		TransportAdapter: boxdns.NewTransportAdapter(C.DNSTypeRFResolverGroup, tag, tags),
		logger:           logger, manager: manager, primaryTags: tags,
	}, nil
}

func (t *Transport) Environment() []string {
	return append([]string{"mode=doh"}, t.primaryTags...)
}

func (t *Transport) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	primaries := make([]adapter.DNSTransport, 0, len(t.primaryTags))
	for _, tag := range t.primaryTags {
		transport, exists := t.manager.Transport(tag)
		if !exists {
			return E.New("rf resolver group[", t.Tag(), "] primary resolver not found: ", tag)
		}
		switch transport.Type() {
		case C.DNSTypeHTTPS, C.DNSTypeHTTP3:
		default:
			return E.New("rf resolver group[", t.Tag(), "] primary resolver[", tag, "] must be https or h3, got ", transport.Type())
		}
		primaries = append(primaries, transport)
	}
	t.primaries = primaries
	return nil
}

func (t *Transport) Close() error { return nil }

func (t *Transport) Reset() {
	for _, primary := range t.primaries {
		primary.Reset()
	}
}

func (t *Transport) Exchange(ctx context.Context, message *mDNS.Msg) (*mDNS.Msg, error) {
	var failures []error
	for _, primary := range t.primaries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		response, err := primary.Exchange(ctx, message.Copy())
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			failures = append(failures, fmt.Errorf("resolver %s: %w", primary.Tag(), err))
			continue
		}
		// A valid DNS response, including SERVFAIL or NXDOMAIN, is the answer
		// to this question. It does not qualify any other name or request.
		t.logger.Debug("rf resolver group[", t.Tag(), "] selected resolver[", primary.Tag(), "]")
		return response, nil
	}
	if len(failures) == 0 {
		return nil, E.New("rf resolver group[", t.Tag(), "] has no initialized primary DoH resolver")
	}
	return nil, errors.Join(failures...)
}

func (t *Transport) ExchangeAsync(ctx context.Context, message *mDNS.Msg, callback func(*mDNS.Msg, error)) {
	go func() {
		response, err := t.Exchange(ctx, message)
		callback(response, err)
	}()
}
