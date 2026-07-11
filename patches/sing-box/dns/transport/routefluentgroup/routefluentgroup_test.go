package routefluentgroup

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	boxdns "github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/require"

	mDNS "github.com/miekg/dns"
)

type fakeManager struct {
	transports map[string]adapter.DNSTransport
}

func (m *fakeManager) Start(stage adapter.StartStage) error { return nil }
func (m *fakeManager) Close() error                         { return nil }
func (m *fakeManager) Transports() []adapter.DNSTransport {
	transports := make([]adapter.DNSTransport, 0, len(m.transports))
	for _, transport := range m.transports {
		transports = append(transports, transport)
	}
	return transports
}
func (m *fakeManager) Transport(tag string) (adapter.DNSTransport, bool) {
	transport, loaded := m.transports[tag]
	return transport, loaded
}
func (m *fakeManager) Default() adapter.DNSTransport { return nil }
func (m *fakeManager) FakeIP() adapter.FakeIPTransport {
	return nil
}
func (m *fakeManager) Remove(tag string) error { return nil }
func (m *fakeManager) Create(ctx context.Context, logger log.ContextLogger, tag string, outboundType string, options any) error {
	return nil
}

type fakeResult struct {
	response *mDNS.Msg
	err      error
}

type fakeTransport struct {
	boxdns.TransportAdapter
	results []fakeResult
	calls   int
}

func newFakeTransport(transportType string, tag string, results ...fakeResult) *fakeTransport {
	return &fakeTransport{
		TransportAdapter: boxdns.NewTransportAdapter(transportType, tag, nil),
		results:          results,
	}
}

func (t *fakeTransport) Start(stage adapter.StartStage) error { return nil }
func (t *fakeTransport) Close() error                         { return nil }
func (t *fakeTransport) Reset()                               {}
func (t *fakeTransport) Exchange(ctx context.Context, message *mDNS.Msg) (*mDNS.Msg, error) {
	t.calls++
	if len(t.results) == 0 {
		return successResponse(message, "192.0.2.1", 600), nil
	}
	result := t.results[0]
	if len(t.results) > 1 {
		t.results = t.results[1:]
	}
	if result.response != nil {
		return result.response.Copy(), result.err
	}
	return nil, result.err
}

func question() *mDNS.Msg {
	message := new(mDNS.Msg)
	message.SetQuestion("proxy.example.", mDNS.TypeA)
	return message
}

func successResponse(request *mDNS.Msg, ip string, ttl uint32) *mDNS.Msg {
	response := new(mDNS.Msg)
	response.SetReply(request)
	response.Rcode = mDNS.RcodeSuccess
	if ip != "" {
		response.Answer = []mDNS.RR{
			&mDNS.A{
				Hdr: mDNS.RR_Header{
					Name:   request.Question[0].Name,
					Rrtype: mDNS.TypeA,
					Class:  mDNS.ClassINET,
					Ttl:    ttl,
				},
				A: netip.MustParseAddr(ip).AsSlice(),
			},
		}
	}
	return response
}

func fixedResponse(request *mDNS.Msg, rcode int) *mDNS.Msg {
	response := new(mDNS.Msg)
	response.SetReply(request)
	response.Rcode = rcode
	return response
}

func testGroup(primary []*primaryState, fallback adapter.DNSTransport, fallbackEnabled bool) *Transport {
	return &Transport{
		TransportAdapter:  boxdns.NewTransportAdapter(C.DNSTypeRouteFluentResolverGroup, "rf-group", nil),
		logger:            log.NewNOPFactory().Logger(),
		primaries:         primary,
		fallback:          fallback,
		fallbackEnabled:   fallbackEnabled,
		failureThreshold:  1,
		fallbackTTLCap:    time.Second,
		unhealthyCooldown: 0,
		closed:            make(chan struct{}),
	}
}

func TestExchangePrefersPrimary(t *testing.T) {
	primary := newFakeTransport(C.DNSTypeHTTPS, "doh", fakeResult{response: successResponse(question(), "192.0.2.10", 600)})
	fallback := newFakeTransport(C.DNSTypeLocal, "local", fakeResult{response: successResponse(question(), "192.0.2.20", 600)})
	group := testGroup([]*primaryState{{tag: "doh", transport: primary, state: stateHealthy}}, fallback, true)

	response, err := group.Exchange(context.Background(), question())
	require.NoError(t, err)
	require.Equal(t, mDNS.RcodeSuccess, response.Rcode)
	require.Equal(t, 1, primary.calls)
	require.Zero(t, fallback.calls)
}

func TestExchangeTriesSecondPrimaryBeforeFallback(t *testing.T) {
	primaryA := newFakeTransport(C.DNSTypeHTTPS, "doh-a", fakeResult{err: errors.New("timeout")})
	primaryB := newFakeTransport(C.DNSTypeHTTPS, "doh-b", fakeResult{response: successResponse(question(), "192.0.2.30", 600)})
	fallback := newFakeTransport(C.DNSTypeLocal, "local", fakeResult{response: successResponse(question(), "192.0.2.40", 600)})
	group := testGroup([]*primaryState{
		{tag: "doh-a", transport: primaryA, state: stateHealthy},
		{tag: "doh-b", transport: primaryB, state: stateHealthy},
	}, fallback, true)

	response, err := group.Exchange(context.Background(), question())
	require.NoError(t, err)
	require.Equal(t, mDNS.RcodeSuccess, response.Rcode)
	require.Equal(t, 1, primaryA.calls)
	require.Equal(t, 1, primaryB.calls)
	require.Zero(t, fallback.calls)
}

func TestExchangeFallsBackWhenAllPrimaryFail(t *testing.T) {
	primary := newFakeTransport(C.DNSTypeHTTPS, "doh", fakeResult{err: errors.New("tls failure")})
	fallback := newFakeTransport(C.DNSTypeLocal, "local", fakeResult{response: successResponse(question(), "192.0.2.50", 600)})
	group := testGroup([]*primaryState{{tag: "doh", transport: primary, state: stateHealthy}}, fallback, true)

	response, err := group.Exchange(context.Background(), question())
	require.NoError(t, err)
	require.Equal(t, mDNS.RcodeSuccess, response.Rcode)
	require.Equal(t, uint32(1), response.Answer[0].Header().Ttl)
	require.Equal(t, 1, primary.calls)
	require.Equal(t, 1, fallback.calls)
}

func TestExchangeFailsClosedWhenFallbackDisabled(t *testing.T) {
	primary := newFakeTransport(C.DNSTypeHTTPS, "doh", fakeResult{err: errors.New("tls failure")})
	fallback := newFakeTransport(C.DNSTypeLocal, "local", fakeResult{response: successResponse(question(), "192.0.2.60", 600)})
	group := testGroup([]*primaryState{{tag: "doh", transport: primary, state: stateHealthy}}, fallback, false)

	_, err := group.Exchange(context.Background(), question())
	require.Error(t, err)
	require.Equal(t, 1, primary.calls)
	require.Zero(t, fallback.calls)
}

func TestExchangeDoesNotFallbackOnNXDOMAIN(t *testing.T) {
	primary := newFakeTransport(C.DNSTypeHTTPS, "doh", fakeResult{response: fixedResponse(question(), mDNS.RcodeNameError)})
	fallback := newFakeTransport(C.DNSTypeLocal, "local", fakeResult{response: successResponse(question(), "192.0.2.70", 600)})
	group := testGroup([]*primaryState{{tag: "doh", transport: primary, state: stateHealthy}}, fallback, true)

	response, err := group.Exchange(context.Background(), question())
	require.NoError(t, err)
	require.Equal(t, mDNS.RcodeNameError, response.Rcode)
	require.Equal(t, 1, primary.calls)
	require.Zero(t, fallback.calls)
}

func TestExchangeRecoversPrimaryAfterFallback(t *testing.T) {
	primary := newFakeTransport(
		C.DNSTypeHTTPS,
		"doh",
		fakeResult{err: errors.New("timeout")},
		fakeResult{response: successResponse(question(), "192.0.2.80", 600)},
	)
	fallback := newFakeTransport(C.DNSTypeLocal, "local", fakeResult{response: successResponse(question(), "192.0.2.90", 600)})
	group := testGroup([]*primaryState{{tag: "doh", transport: primary, state: stateHealthy}}, fallback, true)

	_, err := group.Exchange(context.Background(), question())
	require.NoError(t, err)
	require.Equal(t, 1, fallback.calls)

	response, err := group.Exchange(context.Background(), question())
	require.NoError(t, err)
	require.Equal(t, mDNS.RcodeSuccess, response.Rcode)
	require.Equal(t, 2, primary.calls)
	require.Equal(t, 1, fallback.calls)
}

func TestProbeRecoveryRequiresConfiguredSuccessThreshold(t *testing.T) {
	primary := newFakeTransport(
		C.DNSTypeHTTPS,
		"doh",
		fakeResult{response: successResponse(question(), "192.0.2.91", 600)},
		fakeResult{response: successResponse(question(), "192.0.2.92", 600)},
	)
	fallback := newFakeTransport(C.DNSTypeLocal, "local")
	group := testGroup([]*primaryState{{
		tag:       "doh",
		transport: primary,
		state:     stateUnhealthy,
		nextProbe: time.Now().Add(-time.Second),
	}}, fallback, true)
	group.probeDomains = []string{"probe.example"}
	group.probeInterval = time.Second
	group.recoverySuccessThreshold = 2

	group.probeUnhealthy()
	require.Equal(t, stateUnhealthy, group.primaries[0].state)
	require.Equal(t, 1, group.primaries[0].successes)

	group.primaries[0].nextProbe = time.Now().Add(-time.Second)
	group.probeUnhealthy()
	require.Equal(t, stateHealthy, group.primaries[0].state)
	require.Zero(t, group.primaries[0].successes)
}

func TestLocalOnlyUsesFallbackAndAllowsCache(t *testing.T) {
	fallback := newFakeTransport(C.DNSTypeLocal, "local", fakeResult{response: successResponse(question(), "192.0.2.100", 600)})
	group := testGroup(nil, fallback, true)
	group.localOnly = true

	response, err := group.Exchange(context.Background(), question())
	require.NoError(t, err)
	require.Equal(t, mDNS.RcodeSuccess, response.Rcode)
	require.Equal(t, 1, fallback.calls)
	require.False(t, group.DisableDNSCache())
}

func TestStartRejectsNonDoHPrimaryAndNonLocalFallback(t *testing.T) {
	manager := &fakeManager{transports: map[string]adapter.DNSTransport{
		"udp":   newFakeTransport(C.DNSTypeUDP, "udp"),
		"https": newFakeTransport(C.DNSTypeHTTPS, "https"),
	}}
	ctx := service.ContextWithDefaultRegistry(context.Background())
	service.MustRegister[adapter.DNSTransportManager](ctx, manager)

	transport, err := NewTransport(ctx, log.NewNOPFactory().Logger(), "rf", option.RouteFluentResolverGroupDNSServerOptions{
		Primary:         []string{"udp"},
		Fallback:        "https",
		FallbackEnabled: true,
		ProbeDomains:    []string{"example.com"},
	})
	require.NoError(t, err)
	err = transport.Start(adapter.StartStateStart)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be https or h3")
}
