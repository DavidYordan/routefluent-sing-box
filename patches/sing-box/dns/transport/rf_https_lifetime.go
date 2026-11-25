package transport

import (
	"context"
	"net"

	"github.com/sagernet/sing-box/common/tls"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"golang.org/x/net/http2"
)

// Physical dialing belongs to this wrapper until the resulting connection is
// registered. Close cancels those exact calls as well as established sockets;
// request cancellation retains its own context and metadata throughout.
func (h *HTTPSTransportWrapper) dial(ctx context.Context, dialer N.Dialer, address M.Socksaddr, requireHTTP2 bool) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	h.connAccess.Lock()
	if h.closed {
		h.connAccess.Unlock()
		cancel()
		return nil, net.ErrClosed
	}
	h.dialCancels[&cancel] = struct{}{}
	h.connAccess.Unlock()
	defer func() {
		h.connAccess.Lock()
		delete(h.dialCancels, &cancel)
		h.connAccess.Unlock()
		cancel()
	}()
	conn, err := dialer.DialContext(ctx, N.NetworkTCP, address)
	if err != nil {
		return nil, err
	}
	if requireHTTP2 {
		if tlsConn, ok := conn.(tls.Conn); ok && tlsConn.ConnectionState().NegotiatedProtocol != http2.NextProtoTLS {
			_ = conn.Close()
			h.fallback.Store(true)
			return nil, errFallback
		}
	}
	return h.trackConn(conn)
}
