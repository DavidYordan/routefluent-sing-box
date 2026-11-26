package trafficcontrol

import (
	"context"
	"net"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/sagernet/sing-box/common/rfconnection"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

// The observation follows the connection returned by the actual selection.
// It never reads a selector's later Now() or changes routing or I/O deadlines.
type rfSelection struct {
	chain                          atomic.Pointer[[]string]
	usageMu                        sync.Mutex
	upload, download               *atomic.Int64
	unboundUpload, unboundDownload int64
}

func (s *rfSelection) snapshot() []string {
	if s != nil {
		if chain := s.chain.Load(); chain != nil {
			return slices.Clone(*chain)
		}
	}
	return []string{}
}

func (s *rfSelection) store(chain []string) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if len(s.snapshot()) == 0 && len(chain) != 0 {
		if s.upload != nil {
			s.unboundUpload = s.upload.Load()
		}
		if s.download != nil {
			s.unboundDownload = s.download.Load()
		}
	}
	copy := slices.Clone(chain)
	s.chain.Store(&copy)
}

func (s *rfSelection) usageScope(upload, download int64) ([]string, int64, int64) {
	if s == nil {
		return []string{}, 0, 0
	}
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	return s.snapshot(), min(upload, s.unboundUpload), min(download, s.unboundDownload)
}

type rfSelected interface{ RFSelectedChain() []string }

func rfSelectedChain(conn any) []string {
	if selected, ok := common.Cast[rfSelected](conn); ok {
		return selected.RFSelectedChain()
	}
	return nil
}

func rfAppendSelection(chain, next []string) []string {
	overlap := min(len(chain), len(next))
	for overlap > 0 && !slices.Equal(chain[len(chain)-overlap:], next[:overlap]) {
		overlap--
	}
	return append(chain, next[overlap:]...)
}

type rfSelectedConn struct {
	N.ExtendedConn
	chain []string
}

func (c *rfSelectedConn) RFSelectedChain() []string { return slices.Clone(c.chain) }
func (c *rfSelectedConn) Upstream() any             { return c.ExtendedConn }

func (c *rfSelectedConn) ReaderReplaceable() bool { return true }
func (c *rfSelectedConn) WriterReplaceable() bool { return true }

func RFSelectedConnection(ctx context.Context, conn net.Conn, group, selected string) net.Conn {
	if service.FromContext[*rfconnection.Observer](ctx) == nil {
		return conn
	}
	chain := rfAppendSelection([]string{group, selected}, rfSelectedChain(conn))
	return &rfSelectedConn{ExtendedConn: bufio.NewExtendedConn(conn), chain: chain}
}

type rfSelectedPacketConn struct {
	net.PacketConn
	extended N.PacketConn
	chain    []string
}

func (c *rfSelectedPacketConn) RFSelectedChain() []string { return slices.Clone(c.chain) }
func (c *rfSelectedPacketConn) Upstream() any             { return c.extended }

func (c *rfSelectedPacketConn) ReaderReplaceable() bool { return true }
func (c *rfSelectedPacketConn) WriterReplaceable() bool { return true }
func (c *rfSelectedPacketConn) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	return c.extended.ReadPacket(buffer)
}
func (c *rfSelectedPacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	return c.extended.WritePacket(buffer, destination)
}

func RFSelectedPacketConnection(ctx context.Context, conn net.PacketConn, group, selected string) net.PacketConn {
	if service.FromContext[*rfconnection.Observer](ctx) == nil {
		return conn
	}
	chain := rfAppendSelection([]string{group, selected}, rfSelectedChain(conn))
	return &rfSelectedPacketConn{PacketConn: conn, extended: bufio.NewPacketConn(conn), chain: chain}
}

func rfLocalSelection(conn any) *rfSelection {
	if tracked, ok := common.Cast[*connTracker](conn); ok {
		return tracked.metadata.rfSelection
	}
	if tracked, ok := common.Cast[*packetConnTracker](conn); ok {
		return tracked.metadata.rfSelection
	}
	return nil
}

// The native connection manager calls this with its returned dial winner,
// before packet adapters or copy loops can hide that connection's identity.
// Missing observation leaves attribution unknown; native forwarding continues.
func RFBindSelectedConnection(local, remote any) {
	chain := rfSelectedChain(remote)
	if len(chain) == 0 {
		return
	}
	if selection := rfLocalSelection(local); selection != nil {
		selection.store(rfAppendSelection(selection.snapshot(), chain))
	}
}

// Handler dispatch already holds the chosen outbound. Record that exact call;
// the later native dial may add its nested selection without consulting Now().
func RFRecordSelectedHandler(local any, group, selected string) {
	if selection := rfLocalSelection(local); selection != nil {
		selection.store(rfAppendSelection(selection.snapshot(), []string{group, selected}))
	}
}
