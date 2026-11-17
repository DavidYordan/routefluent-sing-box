// Package rfconnection observes native socket ownership without changing dial
// selection, deadlines, health or connection lifetime.
package rfconnection

import (
	"context"
	"net"
	"sort"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/bufio"
	N "github.com/sagernet/sing/common/network"
)

type Connection struct {
	OutboundTag string `json:"outbound_tag"`
	PeerIPv4    string `json:"peer_ipv4"`
	PeerPort    uint16 `json:"peer_port"`
	Count       int    `json:"count"`
}

type peer struct {
	tag  string
	ipv4 string
	port uint16
}

// Observer belongs to one native instance. Its keys exist only while that
// instance owns sockets, including shared/multiplexed upstream connections.
type Observer struct {
	mu    sync.Mutex
	peers map[peer]int
}

func New() *Observer { return &Observer{peers: make(map[peer]int)} }

func (o *Observer) Track(ctx context.Context, conn net.Conn) net.Conn {
	metadata := adapter.ContextFrom(ctx)
	if metadata == nil || metadata.Outbound == "" {
		return conn
	}
	// A resolver's physical HTTPS socket is not the proxy entry it resolves.
	if _, dns := adapter.DNSTransportTagFromContext(ctx); dns {
		return conn
	}
	address, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok || address.IP.To4() == nil {
		return conn
	}
	key := peer{tag: metadata.Outbound, ipv4: address.IP.To4().String(), port: uint16(address.Port)}
	o.mu.Lock()
	o.peers[key]++
	o.mu.Unlock()
	return &trackedConn{ExtendedConn: bufio.NewExtendedConn(conn), release: func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.peers[key]--; o.peers[key] == 0 {
			delete(o.peers, key)
		}
	}}
}

func (o *Observer) Snapshot() []Connection {
	o.mu.Lock()
	result := make([]Connection, 0, len(o.peers))
	for key, count := range o.peers {
		result = append(result, Connection{OutboundTag: key.tag, PeerIPv4: key.ipv4, PeerPort: key.port, Count: count})
	}
	o.mu.Unlock()
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.OutboundTag != b.OutboundTag {
			return a.OutboundTag < b.OutboundTag
		}
		if a.PeerIPv4 != b.PeerIPv4 {
			return a.PeerIPv4 < b.PeerIPv4
		}
		return a.PeerPort < b.PeerPort
	})
	return result
}

type trackedConn struct {
	N.ExtendedConn
	once     sync.Once
	release  func()
	closeErr error
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.closeErr = c.ExtendedConn.Close()
		c.release()
	})
	return c.closeErr
}

func (c *trackedConn) Upstream() any { return c.ExtendedConn }
