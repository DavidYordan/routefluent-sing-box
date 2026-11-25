package group

import (
	"context"
	"io"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/interrupt"
	"github.com/sagernet/sing-box/common/rfconnection"
	"github.com/sagernet/sing-box/common/trafficcontrol"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/route"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

type rfSelectionOutbound struct {
	adapter.Outbound
	tag        string
	dials      atomic.Int32
	beforeDial func()
}

func (o *rfSelectionOutbound) Type() string { return "rf-selection-fixture" }
func (o *rfSelectionOutbound) Tag() string  { return o.tag }
func (o *rfSelectionOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	o.dials.Add(1)
	if o.beforeDial != nil {
		o.beforeDial()
	}
	return (&net.Dialer{}).DialContext(ctx, network, destination.String())
}
func (o *rfSelectionOutbound) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	o.dials.Add(1)
	return net.ListenPacket("udp", "127.0.0.1:0")
}

type rfSelectionOutbounds struct {
	adapter.OutboundManager
	values map[string]adapter.Outbound
}

func (m rfSelectionOutbounds) Outbound(tag string) (adapter.Outbound, bool) {
	value, ok := m.values[tag]
	return value, ok
}

func rfSelectionFixture() (*Selector, *rfSelectionOutbound, *rfSelectionOutbound, *trafficcontrol.Manager) {
	x := &rfSelectionOutbound{tag: "route-x"}
	y := &rfSelectionOutbound{tag: "route-y"}
	selector := &Selector{Adapter: outbound.NewAdapter("selector", "route-selector", nil, nil), interruptGroup: interrupt.NewGroup()}
	selector.selected.Store(x)
	manager := trafficcontrol.NewManager(rfSelectionOutbounds{values: map[string]adapter.Outbound{"route-selector": selector, x.Tag(): x, y.Tag(): y}})
	return selector, x, y, manager
}

func rfAssertSelection(t *testing.T, manager *trafficcontrol.Manager, want []string, progressed bool) {
	t.Helper()
	value, err := manager.RFActivitySnapshot()
	// A socket read can finish before the native writer increments its counter.
	// Wait for that already-running copy, never infer progress from client I/O.
	deadline := time.Now().Add(time.Second)
	for progressed && err == nil && len(value.Flows) == 1 && (value.Flows[0].Upload == 0 || value.Flows[0].Download == 0) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		value, err = manager.RFActivitySnapshot()
	}
	if err != nil || len(value.Flows) != 1 || !slices.Equal(value.Flows[0].Chain, want) {
		t.Fatalf("actual native selection: %#v, %v; want %v", value, err, want)
	}
	if progressed && (value.Flows[0].Upload == 0 || value.Flows[0].Download == 0) {
		t.Fatal("real bidirectional bytes were lost", value)
	}
}

func TestRFSelectedTCPUsesNativeDialWinnerAfterSelectorChanges(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		conn, err := target.Accept()
		if err == nil {
			defer conn.Close()
			_, _ = io.Copy(conn, conn)
		}
	}()
	selector, x, y, manager := rfSelectionFixture()
	client, local := net.Pipe()
	defer client.Close()
	ctx := service.ContextWith(t.Context(), rfconnection.New())
	metadata := adapter.InboundContext{Destination: M.ParseSocksaddr(target.Addr().String())}
	tracked := manager.RoutedConnection(ctx, local, metadata, nil, selector)
	rfAssertSelection(t, manager, nil, false)
	selector.selected.Store(y)
	closed := make(chan struct{})
	native := route.NewConnectionManager(log.NewNOPFactory().Logger())
	dialStarted, releaseDial, routed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	y.beforeDial = func() { close(dialStarted); <-releaseDial }
	go func() {
		native.NewConnection(ctx, selector, tracked, metadata, func(error) { close(closed) })
		close(routed)
	}()
	select {
	case <-dialStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("native selected dial did not start")
	}
	selector.selected.Store(x)
	close(releaseDial)
	select {
	case <-routed:
	case <-time.After(3 * time.Second):
		t.Fatal("native selected dial did not return")
	}
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := client.Write([]byte("native-tcp")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len("native-tcp"))
	if _, err := io.ReadFull(client, response); err != nil || string(response) != "native-tcp" {
		t.Fatalf("native TCP copy changed: %q, %v", response, err)
	}
	rfAssertSelection(t, manager, []string{"route-selector", "route-y"}, true)
	if x.dials.Load() != 0 || y.dials.Load() != 1 {
		t.Fatal("observer changed the actual selector choice")
	}
	_ = client.Close()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("native TCP copy did not retire")
	}
	<-echoDone
	value, err := manager.RFActivitySnapshot()
	if err != nil || len(value.Flows) != 0 {
		t.Fatal("closed native selection retained a flow", value, err)
	}
}

type rfTargetPacket struct {
	N.PacketConn
	target M.Socksaddr
	client M.Socksaddr
}

func (c *rfTargetPacket) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	_, err := c.PacketConn.ReadPacket(buffer)
	return c.target, err
}
func (c *rfTargetPacket) WritePacket(buffer *buf.Buffer, _ M.Socksaddr) error {
	return c.PacketConn.WritePacket(buffer, c.client)
}

func TestRFSelectedUDPUsesNativeReturnedSocketForBothDialModes(t *testing.T) {
	for _, connected := range []bool{false, true} {
		t.Run(map[bool]string{false: "packet-listen", true: "udp-connect"}[connected], func(t *testing.T) {
			target, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			echoDone := make(chan struct{})
			go func() {
				defer close(echoDone)
				buffer := make([]byte, 128)
				for {
					n, from, err := target.ReadFrom(buffer)
					if err != nil {
						return
					}
					_, _ = target.WriteTo(buffer[:n], from)
				}
			}()
			local, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer local.Close()
			client, err := net.Dial("udp", local.LocalAddr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			selector, x, y, manager := rfSelectionFixture()
			metadata := adapter.InboundContext{Destination: M.ParseSocksaddr(target.LocalAddr().String()), UDPConnect: connected}
			inbound := &rfTargetPacket{PacketConn: bufio.NewPacketConn(local), target: metadata.Destination, client: M.SocksaddrFromNet(client.LocalAddr())}
			ctx := service.ContextWith(t.Context(), rfconnection.New())
			tracked := manager.RoutedPacketConnection(ctx, inbound, metadata, nil, selector)
			rfAssertSelection(t, manager, nil, false)
			selector.selected.Store(y)
			closed := make(chan struct{})
			native := route.NewConnectionManager(log.NewNOPFactory().Logger())
			native.NewPacketConnection(ctx, selector, tracked, metadata, func(error) { close(closed) })
			selector.selected.Store(x)
			_ = client.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := client.Write([]byte("native-udp")); err != nil {
				t.Fatal(err)
			}
			response := make([]byte, 128)
			n, err := client.Read(response)
			if err != nil || string(response[:n]) != "native-udp" {
				t.Fatalf("native UDP copy changed: %q, %v", response[:n], err)
			}
			rfAssertSelection(t, manager, []string{"route-selector", "route-y"}, true)
			if x.dials.Load() != 0 || y.dials.Load() != 1 {
				t.Fatal("observer changed native UDP selection")
			}
			_ = local.Close()
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				t.Fatal("native UDP copy did not retire")
			}
			_ = target.Close()
			<-echoDone
		})
	}
}
