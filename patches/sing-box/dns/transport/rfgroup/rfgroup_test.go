package rfgroup

import (
	"context"
	"net/netip"
	"testing"

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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

func (t *fakeTransport) ExchangeAsync(ctx context.Context, message *mDNS.Msg, callback func(response *mDNS.Msg, err error)) {
	response, err := t.Exchange(ctx, message)
	callback(response, err)
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

func TestTransportErrorsTryDeclaredPeersForThisRequestOnly(t *testing.T) {
	first := newFakeTransport(C.DNSTypeHTTPS, "first", fakeResult{err: context.DeadlineExceeded})
	second := newFakeTransport(C.DNSTypeHTTPS, "second")
	group := configuredTestGroup(t, first, second)
	before := group.Environment()
	for range 3 {
		response, err := group.Exchange(context.Background(), question())
		require.NoError(t, err)
		require.Equal(t, mDNS.RcodeSuccess, response.Rcode)
	}
	require.Equal(t, 3, first.calls)
	require.Equal(t, 3, second.calls)
	require.Equal(t, before, group.Environment())
}

func TestPrimaryFailureKeepsCauseAndDoesNotChangeCacheAuthority(t *testing.T) {
	primary := newFakeTransport(C.DNSTypeHTTPS, "doh", fakeResult{err: context.DeadlineExceeded})
	group := configuredTestGroup(t, primary)
	before := group.Environment()
	for range 3 {
		response, err := group.Exchange(context.Background(), question())
		require.Nil(t, response)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
	require.Equal(t, 3, primary.calls)
	require.Equal(t, before, group.Environment())
}

func TestNXDOMAINIsReturnedWithoutAnotherResolver(t *testing.T) {
	first := newFakeTransport(C.DNSTypeHTTPS, "first", fakeResult{response: fixedResponse(question(), mDNS.RcodeNameError)})
	second := newFakeTransport(C.DNSTypeHTTPS, "second")
	group := configuredTestGroup(t, first, second)
	response, err := group.Exchange(context.Background(), question())
	require.NoError(t, err)
	require.Equal(t, mDNS.RcodeNameError, response.Rcode)
	require.Zero(t, second.calls)
}

func TestNativeStartRejectsLocalAsADoHPrimary(t *testing.T) {
	manager := &fakeManager{transports: map[string]adapter.DNSTransport{
		"local": newFakeTransport(C.DNSTypeLocal, "local"),
	}}
	ctx := service.ContextWithDefaultRegistry(context.Background())
	service.MustRegister[adapter.DNSTransportManager](ctx, manager)
	group, err := NewTransport(ctx, log.NewNOPFactory().Logger(), "group", option.RFResolverGroupDNSServerOptions{Primary: []string{"local"}})
	require.NoError(t, err)
	require.ErrorContains(t, group.Start(adapter.StartStateStart), "must be https or h3")
}

func TestGroupWithoutNativeStartCannotReturnEmptySuccess(t *testing.T) {
	manager := &fakeManager{transports: map[string]adapter.DNSTransport{}}
	ctx := service.ContextWithDefaultRegistry(context.Background())
	service.MustRegister[adapter.DNSTransportManager](ctx, manager)
	group, err := NewTransport(ctx, log.NewNOPFactory().Logger(), "group", option.RFResolverGroupDNSServerOptions{Primary: []string{"doh"}})
	require.NoError(t, err)
	response, err := group.Exchange(context.Background(), question())
	require.Nil(t, response)
	require.ErrorContains(t, err, "no initialized primary")
}
