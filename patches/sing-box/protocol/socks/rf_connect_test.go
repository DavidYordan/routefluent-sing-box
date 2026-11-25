package socks

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/rfconnection"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/varbin"
	client "github.com/sagernet/sing/protocol/socks"
	"github.com/sagernet/sing/protocol/socks/socks5"
	"github.com/sagernet/sing/service"
)

type rfConnectDialer struct{}

func (rfConnectDialer) DialContext(ctx context.Context, network string, address M.Socksaddr) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, address.String())
}
func (rfConnectDialer) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("UDP is outside the TCP CONNECT fixture")
}

func TestRFSOCKS5ConnectUsesReplyAndKeepsFailureAcrossCancellationAndSuccess(t *testing.T) {
	observer := rfconnection.New()
	ctx := service.ContextWith(t.Context(), observer)
	ctx = rfconnection.WithConnectSelection(ctx, "actual-selector", "actual-socks")
	for _, mode := range []string{"rejected", "canceled", "accepted"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			ready, release := make(chan struct{}), make(chan struct{})
			serverDone := make(chan error, 1)
			go func() {
				peer, err := listener.Accept()
				if err != nil {
					serverDone <- err
					return
				}
				defer peer.Close()
				_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
				reader := varbin.StubReader(peer)
				if _, err := socks5.ReadAuthRequest(reader); err != nil {
					serverDone <- err
					return
				}
				if err := socks5.WriteAuthResponse(peer, socks5.AuthResponse{Method: 0}); err != nil {
					serverDone <- err
					return
				}
				request, err := socks5.ReadRequest(reader)
				if err != nil {
					serverDone <- err
					return
				}
				if request.Command != socks5.CommandConnect {
					serverDone <- errors.New("not CONNECT")
					return
				}
				close(ready)
				select {
				case <-release:
					code := byte(socks5.ReplyCodeSuccess)
					if mode == "rejected" {
						code = 5
					}
					serverDone <- socks5.WriteResponse(peer, socks5.Response{ReplyCode: code, Bind: M.ParseSocksaddr("127.0.0.1:12345")})
				case <-ctx.Done():
					serverDone <- nil
				}
			}()
			proxy := &Outbound{Adapter: outbound.NewAdapter("socks", "actual-socks", nil, nil), logger: log.NewNOPFactory().Logger(), client: client.NewClient(rfConnectDialer{}, M.SocksaddrFromNet(listener.Addr()), client.Version5, "", ""), rfSOCKS5: true}
			type outcome struct {
				conn net.Conn
				err  error
			}
			done := make(chan outcome, 1)
			go func() {
				conn, err := proxy.DialContext(ctx, "tcp", M.ParseSocksaddr("198.51.100.23:443"))
				done <- outcome{conn, err}
			}()
			select {
			case <-ready:
			case err := <-serverDone:
				t.Fatal("server did not receive CONNECT", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			before := observer.ConnectSnapshot("epoch-one")
			var completed uint64
			for _, entry := range before.Entries {
				completed += entry.Succeeded + entry.Failed + entry.Canceled
			}
			if (mode == "rejected" && completed != 0) || (mode == "canceled" && completed != 1) || (mode == "accepted" && completed != 2) {
				t.Fatal("TCP alone published a CONNECT completion", before)
			}
			// Later selection contexts cannot replace this call's captured one.
			_ = rfconnection.WithConnectSelection(ctx, "later-selector", "later-socks")
			if mode == "canceled" {
				cancel()
			} else {
				close(release)
			}
			result := <-done
			if result.conn != nil {
				defer result.conn.Close()
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
			value := observer.ConnectSnapshot("epoch-one")
			if len(value.Entries) != 1 {
				t.Fatal(value)
			}
			entry := value.Entries[0]
			if !slices.Equal(entry.Chain, []string{"actual-selector", "actual-socks"}) || entry.OutboundTag != "actual-socks" || entry.Failed != 1 || entry.LastFailure == nil || !strings.Contains(entry.LastFailure.Error, "code=5") {
				t.Fatal("failure or captured selection lost", entry)
			}
			switch mode {
			case "rejected":
				if result.err == nil || entry.Latest == nil || entry.Latest.Outcome != "failed" {
					t.Fatal(entry, result.err)
				}
			case "canceled":
				if !errors.Is(result.err, context.Canceled) || entry.Canceled != 1 || entry.Latest.Outcome != "failed" || entry.LastCanceled == nil {
					t.Fatal("cancellation manufactured recovery", entry, result.err)
				}
			case "accepted":
				if result.err != nil || entry.Succeeded != 1 || entry.Latest.Outcome != "accepted" {
					t.Fatal(entry, result.err)
				}
				if _, err := result.conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
					t.Fatal("fixture should supply no application response", err)
				}
			}
			entry.Chain[0] = "mutated-reader-copy"
			entry.LastFailure.Error = "mutated-reader-copy"
			if fresh := observer.ConnectSnapshot("epoch-one").Entries[0]; fresh.Chain[0] != "actual-selector" || !strings.Contains(fresh.LastFailure.Error, "code=5") {
				t.Fatal("reader rewrote native facts", fresh)
			}
		})
	}
	if fresh := rfconnection.New().ConnectSnapshot("epoch-two"); len(fresh.Entries) != 0 {
		t.Fatal("new instance inherited old counters", fresh)
	}
}
