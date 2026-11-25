package transport

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mDNS "github.com/miekg/dns"
	M "github.com/sagernet/sing/common/metadata"
)

func TestRFHTTPSHTTP1RequestEndsItsPendingPhysicalDial(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name, expected := "cancel", context.Canceled
		if deadline {
			name, expected = "deadline", context.DeadlineExceeded
		}
		t.Run(name, func(t *testing.T) {
			dialer := &rfHTTPSBlockingDialer{started: make(chan struct{}), stopped: make(chan struct{}), release: make(chan struct{})}
			fallback := new(atomic.Bool)
			fallback.Store(true)
			wrapper := NewHTTPSTransportWrapper(dialer, M.ParseSocksaddr("fixture.example:443"), fallback)
			ctx, cancel := context.WithCancel(t.Context())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
			}
			t.Cleanup(func() { cancel(); close(dialer.release); wrapper.Close() })
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.example/dns-query", nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := wrapper.RoundTrip(request); done <- err }()
			<-dialer.started
			if !deadline {
				cancel()
			}
			if err := <-done; !errors.Is(err, expected) {
				t.Fatalf("original request lost its failure: %v", err)
			}
			select {
			case <-dialer.stopped:
			case <-time.After(time.Second):
				t.Fatal("HTTP1 request returned but its native DoH physical dial kept running")
			}
		})
	}
}

type rfHTTPSHTTP1MetadataKey struct{}

type rfHTTPSHTTP1SelectiveDialer struct {
	rfHTTPSFixtureDialer
	slow *rfHTTPSBlockingDialer
}

func (d rfHTTPSHTTP1SelectiveDialer) DialContext(ctx context.Context, network string, address M.Socksaddr) (net.Conn, error) {
	switch ctx.Value(rfHTTPSHTTP1MetadataKey{}) {
	case "slow":
		return d.slow.DialContext(ctx, network, address)
	case "healthy":
		return d.rfHTTPSFixtureDialer.DialContext(ctx, network, address)
	default:
		return nil, errors.New("native DoH lost this query's context metadata")
	}
}

func TestRFHTTPSHTTP1CanceledDialKeepsHealthyQueryAndMetadata(t *testing.T) {
	healthyStarted, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseHealthy := func() { releaseOnce.Do(func() { close(release) }) }
	transport, connections := rfHTTPSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Error("fixture did not consume actual HTTP1")
		}
		if question := rfHTTPSQuestion(t, r); question != nil {
			if question.Question[0].Name == "healthy.example." {
				close(healthyStarted)
				<-release
			}
			rfHTTPSReply(t, w, question)
		}
	}))
	t.Cleanup(releaseHealthy)
	baseDialer := transport.dialer.(rfHTTPSFixtureDialer)
	baseDialer.config = baseDialer.config.Clone()
	baseDialer.config.NextProtos = []string{"http/1.1"}
	slowDialer := &rfHTTPSBlockingDialer{started: make(chan struct{}), stopped: make(chan struct{}), release: make(chan struct{})}
	transport.dialer = rfHTTPSHTTP1SelectiveDialer{baseDialer, slowDialer}
	transport.fallback.Store(true)
	transport.Reset()
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), rfHTTPSHTTP1MetadataKey{}, "slow"))
	t.Cleanup(func() { cancel(); close(slowDialer.release) })
	slowQuestion := new(mDNS.Msg)
	slowQuestion.SetQuestion("slow.example.", mDNS.TypeA)
	slow := make(chan error, 1)
	go func() { _, err := transport.Exchange(ctx, slowQuestion); slow <- err }()
	<-slowDialer.started
	healthyCtx, healthyCancel := context.WithTimeout(context.WithValue(t.Context(), rfHTTPSHTTP1MetadataKey{}, "healthy"), 2*time.Second)
	defer healthyCancel()
	question := new(mDNS.Msg)
	question.SetQuestion("healthy.example.", mDNS.TypeA)
	type queryResult struct {
		answer *mDNS.Msg
		err    error
	}
	healthy := make(chan queryResult, 1)
	go func() { answer, err := transport.Exchange(healthyCtx, question); healthy <- queryResult{answer, err} }()
	select {
	case <-healthyStarted:
	case <-healthyCtx.Done():
		t.Fatal("healthy HTTP1 query did not reach the real TLS consumer")
	}
	// Keep this connection busy in its own healthy request. An HTTP1 transport
	// may otherwise assign an already-idle connection to the waiting slow query.
	cancel()
	if err := <-slow; !errors.Is(err, context.Canceled) {
		t.Fatalf("slow query lost original cancellation: %v", err)
	}
	select {
	case <-slowDialer.stopped:
	case <-time.After(time.Second):
		t.Fatal("canceled HTTP1 query kept dialing while its sibling was active")
	}
	releaseHealthy()
	result := <-healthy
	if result.err != nil || result.answer == nil || len(result.answer.Answer) != 1 {
		t.Fatalf("concurrent HTTP1 query failed: answer=%v error=%v", result.answer, result.err)
	}
	question.SetQuestion("later.example.", mDNS.TypeA)
	answer, err := transport.Exchange(healthyCtx, question)
	if err != nil || answer == nil || len(answer.Answer) != 1 || connections.opened.Load() != 1 {
		t.Fatalf("healthy query's connection was not reusable: answer=%v error=%v TLS connections=%d", answer, err, connections.opened.Load())
	}
}
