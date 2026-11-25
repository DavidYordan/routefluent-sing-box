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
