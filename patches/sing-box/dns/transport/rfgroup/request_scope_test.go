package rfgroup

import (
	"context"
	"testing"

	mDNS "github.com/miekg/dns"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/require"
)

func configuredTestGroup(t *testing.T, primaries ...*fakeTransport) *Transport {
	t.Helper()
	manager := &fakeManager{transports: map[string]adapter.DNSTransport{}}
	tags := make([]string, 0, len(primaries))
	for _, primary := range primaries {
		manager.transports[primary.Tag()] = primary
		tags = append(tags, primary.Tag())
	}
	ctx := service.ContextWithDefaultRegistry(context.Background())
	service.MustRegister[adapter.DNSTransportManager](ctx, manager)
	group, err := NewTransport(ctx, log.NewNOPFactory().Logger(), "group", option.RFResolverGroupDNSServerOptions{Primary: tags})
	require.NoError(t, err)
	require.NoError(t, group.Start(adapter.StartStateStart))
	t.Cleanup(func() { require.NoError(t, group.Close()) })
	return group.(*Transport)
}

func TestCanceledRequestsCannotDisableNextDNSRequest(t *testing.T) {
	primary := newFakeTransport(C.DNSTypeHTTPS, "doh")
	group := configuredTestGroup(t, primary)
	before := group.Environment()
	for range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := group.Exchange(ctx, question())
		require.Error(t, err)
	}
	calls := primary.calls
	request := new(mDNS.Msg)
	request.SetQuestion("another.example.", mDNS.TypeA)
	response, err := group.Exchange(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, mDNS.RcodeSuccess, response.Rcode)
	require.Equal(t, calls+1, primary.calls)
	require.Equal(t, before, group.Environment())
}

func TestSERVFAILIsTheExactResponseAndCannotDisableAnotherName(t *testing.T) {
	primary := newFakeTransport(C.DNSTypeHTTPS, "doh",
		fakeResult{response: fixedResponse(question(), mDNS.RcodeServerFailure)},
		fakeResult{response: fixedResponse(question(), mDNS.RcodeServerFailure)},
		fakeResult{response: successResponse(question(), "192.0.2.80", 600)})
	other := newFakeTransport(C.DNSTypeHTTPS, "other")
	group := configuredTestGroup(t, primary, other)
	before := group.Environment()
	for range 2 {
		response, err := group.Exchange(context.Background(), question())
		require.NoError(t, err)
		require.Equal(t, mDNS.RcodeServerFailure, response.Rcode)
	}
	response, err := group.Exchange(context.Background(), question())
	require.NoError(t, err)
	require.Equal(t, mDNS.RcodeSuccess, response.Rcode)
	require.Equal(t, 3, primary.calls)
	require.Zero(t, other.calls)
	require.Equal(t, before, group.Environment())
}
