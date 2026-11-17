package rfconnection

import (
	"context"
	"net"
	"testing"

	"github.com/sagernet/sing-box/adapter"
)

func socketPair(t *testing.T) net.Conn {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { accepted.Close() })
	return conn
}

func TestObserverTracksActualPeerAndOutboundForSocketLifetime(t *testing.T) {
	observer := New()
	metadata := &adapter.InboundContext{Outbound: "entry-a"}
	ctx := adapter.WithContext(t.Context(), metadata)
	conn := socketPair(t)
	peer := conn.RemoteAddr().(*net.TCPAddr)
	first := observer.Track(ctx, conn)
	t.Cleanup(func() { first.Close() })
	metadata.Outbound = "entry-b"
	second := observer.Track(ctx, socketPair(t))
	t.Cleanup(func() { second.Close() })
	values := observer.Snapshot()
	if len(values) != 2 || values[0].OutboundTag != "entry-a" || values[1].OutboundTag != "entry-b" ||
		values[0].PeerIPv4 != peer.IP.String() || values[0].PeerPort != uint16(peer.Port) || values[0].Count != 1 {
		t.Fatalf("native socket attribution changed: %+v", values)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if values := observer.Snapshot(); len(values) != 1 || values[0].OutboundTag != "entry-b" {
		t.Fatalf("closed socket leaked or removed its sibling: %+v", values)
	}
	if len(New().Snapshot()) != 0 {
		t.Fatal("another instance inherited old sockets")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if len(observer.Snapshot()) != 0 || len(observer.peers) != 0 {
		t.Fatal("last socket retained observation history")
	}
}

func TestObserverLeavesUnknownAndDNSConnectionsUnattributed(t *testing.T) {
	observer := New()
	conn := socketPair(t)
	metadata := &adapter.InboundContext{Outbound: "entry-a"}
	for _, ctx := range []context.Context{
		t.Context(),
		adapter.WithContext(t.Context(), &adapter.InboundContext{}),
		adapter.ContextWithDNSTransportTag(adapter.WithContext(t.Context(), metadata), "entry-resolver"),
	} {
		if observer.Track(ctx, conn) != conn {
			t.Fatal("unknown/DNS socket acquired a production outbound observation")
		}
	}
	pipe, other := net.Pipe()
	defer pipe.Close()
	defer other.Close()
	if observer.Track(adapter.WithContext(t.Context(), metadata), pipe) != pipe {
		t.Fatal("non-TCP peer became an IPv4 observation")
	}
	if len(observer.Snapshot()) != 0 {
		t.Fatal("unattributed sockets leaked into the snapshot")
	}
}
