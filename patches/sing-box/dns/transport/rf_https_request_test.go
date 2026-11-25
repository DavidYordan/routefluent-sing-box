package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mDNS "github.com/miekg/dns"
	M "github.com/sagernet/sing/common/metadata"
)

type rfHTTPSFixtureDialer struct{ config *tls.Config }

func (d rfHTTPSFixtureDialer) DialContext(ctx context.Context, network string, address M.Socksaddr) (net.Conn, error) {
	return (&tls.Dialer{NetDialer: &net.Dialer{}, Config: d.config}).DialContext(ctx, network, address.String())
}

func (d rfHTTPSFixtureDialer) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("this HTTPS fixture has no packet listener")
}

type rfHTTPSFixtureConnections struct {
	opened atomic.Int32
	closed chan struct{}
}

func rfHTTPSFixture(t *testing.T, handler http.Handler) (*HTTPSTransport, *rfHTTPSFixtureConnections) {
	t.Helper()
	connections := &rfHTTPSFixtureConnections{closed: make(chan struct{}, 8)}
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.opened.Add(1)
		}
		if state == http.StateClosed {
			connections.closed <- struct{}{}
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	destination, err := url.Parse(server.URL + "/dns-query")
	if err != nil {
		t.Fatal(err)
	}
	transport := &HTTPSTransport{
		dialer:      rfHTTPSFixtureDialer{&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}}},
		destination: destination, headers: make(http.Header), serverAddr: M.ParseSocksaddr(destination.Host), fallback: new(atomic.Bool),
	}
	transport.transport = NewHTTPSTransportWrapper(transport.dialer, transport.serverAddr, transport.fallback)
	t.Cleanup(func() { _ = transport.Close() })
	return transport, connections
}

func rfHTTPSQuestion(t *testing.T, r *http.Request) *mDNS.Msg {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil
	}
	var request mDNS.Msg
	if err := request.Unpack(raw); err != nil {
		t.Errorf("decode actual DoH request: %v", err)
		return nil
	}
	return &request
}

func rfHTTPSReply(t *testing.T, w http.ResponseWriter, request *mDNS.Msg) {
	t.Helper()
	response := new(mDNS.Msg)
	response.SetReply(request)
	answer, err := mDNS.NewRR(request.Question[0].Name + " 60 IN A 192.0.2.10")
	if err != nil {
		t.Error(err)
		return
	}
	response.Answer = []mDNS.RR{answer}
	raw, err := response.Pack()
	if err != nil {
		t.Error(err)
		return
	}
	w.Header().Set("Content-Type", MimeType)
	_, _ = w.Write(raw)
}

// The two real DoH exchanges share one HTTP/2 connection. A slow request's
// deadline must cancel that stream without closing the healthy sibling stream
// or forcing a later DNS request through a new TLS connection.
func TestRFHTTPSDeadlinePreservesSiblingAndReusableConnection(t *testing.T) {
	slowStarted, healthyStarted := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHealthy := func() { releaseOnce.Do(func() { close(release) }) }
	transport, connections := rfHTTPSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Error("fixture did not negotiate actual HTTP/2")
		}
		request := rfHTTPSQuestion(t, r)
		if request == nil {
			return
		}
		if request.Question[0].Name == "slow.example." {
			close(slowStarted)
			<-r.Context().Done()
			return
		}
		if request.Question[0].Name == "healthy.example." {
			close(healthyStarted)
			<-release
		}
		rfHTTPSReply(t, w, request)
	}))
	t.Cleanup(releaseHealthy)
	slowRequest := new(mDNS.Msg)
	slowRequest.SetQuestion("slow.example.", mDNS.TypeA)
	healthyRequest := new(mDNS.Msg)
	healthyRequest.SetQuestion("healthy.example.", mDNS.TypeA)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	slow := make(chan error, 1)
	go func() { _, err := transport.Exchange(ctx, slowRequest); slow <- err }()
	<-slowStarted
	healthy := make(chan error, 1)
	go func() { _, err := transport.Exchange(t.Context(), healthyRequest); healthy <- err }()
	<-healthyStarted
	if connections.opened.Load() != 1 {
		t.Fatalf("requests did not share one TLS connection: %d", connections.opened.Load())
	}
	if err := <-slow; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("original request lost its deadline failure: %v", err)
	}
	releaseHealthy()
	if err := <-healthy; err != nil {
		t.Fatalf("one query deadline closed a healthy sibling: %v", err)
	}
	later := new(mDNS.Msg)
	later.SetQuestion("later.example.", mDNS.TypeA)
	if response, err := transport.Exchange(t.Context(), later); err != nil || len(response.Answer) != 1 {
		t.Fatalf("later DNS request failed: response=%v error=%v", response, err)
	}
	if connections.opened.Load() != 1 {
		t.Fatalf("request timeout unnecessarily replaced shared TLS: %d connections", connections.opened.Load())
	}
}

func TestRFHTTPSExplicitResetStillClosesItsConnections(t *testing.T) {
	transport, connections := rfHTTPSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if request := rfHTTPSQuestion(t, r); request != nil {
			rfHTTPSReply(t, w, request)
		}
	}))
	request := new(mDNS.Msg)
	request.SetQuestion("fixture.example.", mDNS.TypeA)
	if _, err := transport.Exchange(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	transport.Reset()
	select {
	case <-connections.closed:
	case <-time.After(time.Second):
		t.Fatal("explicit native reset left its original TLS connection open")
	}
	if _, err := transport.Exchange(t.Context(), request); err != nil {
		t.Fatalf("new request after explicit reset failed: %v", err)
	}
	if connections.opened.Load() != 2 {
		t.Fatalf("explicit reset did not use a new TLS connection: %d", connections.opened.Load())
	}
}

func TestRFHTTPSCloseCannotAcceptAnotherQuery(t *testing.T) {
	transport, connections := rfHTTPSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if request := rfHTTPSQuestion(t, r); request != nil {
			rfHTTPSReply(t, w, request)
		}
	}))
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	// An environment callback after retirement cannot revive this owner.
	transport.Reset()
	request := new(mDNS.Msg)
	request.SetQuestion("closed.example.", mDNS.TypeA)
	_, err := transport.Exchange(t.Context(), request)
	if !errors.Is(err, net.ErrClosed) || connections.opened.Load() != 0 {
		t.Fatalf("closed native DoH accepted work: error=%v physical TLS connections=%d", err, connections.opened.Load())
	}
}

type rfHTTPSBlockingDialer struct {
	started, stopped, release chan struct{}
	startOnce, stopOnce       sync.Once
	calls                     atomic.Int32
}

func (d *rfHTTPSBlockingDialer) DialContext(ctx context.Context, _ string, _ M.Socksaddr) (net.Conn, error) {
	d.calls.Add(1)
	d.startOnce.Do(func() { close(d.started) })
	select {
	case <-ctx.Done():
		d.stopOnce.Do(func() { close(d.stopped) })
		return nil, ctx.Err()
	case <-d.release:
		return nil, errors.New("fixture dial released")
	}
}

func (d *rfHTTPSBlockingDialer) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("this HTTPS fixture has no packet listener")
}

func TestRFHTTPSCloseEndsPendingPhysicalDial(t *testing.T) {
	for _, http1 := range []bool{false, true} {
		name := "HTTP2"
		if http1 {
			name = "HTTP1"
		}
		t.Run(name, func(t *testing.T) {
			dialer := &rfHTTPSBlockingDialer{started: make(chan struct{}), stopped: make(chan struct{}), release: make(chan struct{})}
			fallback := new(atomic.Bool)
			fallback.Store(http1)
			wrapper := NewHTTPSTransportWrapper(dialer, M.ParseSocksaddr("fixture.example:443"), fallback)
			ctx, cancel := context.WithCancel(t.Context())
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.example/dns-query", nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := wrapper.RoundTrip(request); done <- err }()
			t.Cleanup(func() {
				cancel()
				close(dialer.release)
				wrapper.Close()
			})
			<-dialer.started
			wrapper.Close()
			select {
			case <-dialer.stopped:
			case <-time.After(time.Second):
				t.Fatal("physical dial outlived the native DoH transport Close")
			}
			if err := <-done; err == nil {
				t.Fatal("retired native transport returned empty success")
			}
		})
	}
}

func TestRFHTTPSClosedWrapperRejectsBeforePhysicalDial(t *testing.T) {
	dialer := &rfHTTPSBlockingDialer{started: make(chan struct{}), stopped: make(chan struct{}), release: make(chan struct{})}
	close(dialer.release)
	wrapper := NewHTTPSTransportWrapper(dialer, M.ParseSocksaddr("fixture.example:443"), new(atomic.Bool))
	wrapper.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://fixture.example/dns-query", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = wrapper.RoundTrip(request)
	if !errors.Is(err, net.ErrClosed) || dialer.calls.Load() != 0 {
		t.Fatalf("closed wrapper entered physical dialing: error=%v dials=%d", err, dialer.calls.Load())
	}
}
