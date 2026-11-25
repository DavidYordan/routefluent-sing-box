package transport

import (
	"context"
	"net"

	"github.com/sagernet/sing-box/common/tls"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"golang.org/x/net/http2"
)

type httpsDialOwnerKey struct{}

// net/http keeps speculative HTTP1 dialing alive after its request returns.
// DoH owns that work through the original query, retaining transport cancellation
// and all request metadata without holding it for another query.
func httpsHTTP1DialContext(ctx context.Context) (context.Context, context.CancelFunc) {
	// The wrapper's sole RoundTrip entry attaches this typed owner before
	// either HTTP transport sees the request; net/http preserves its values.
	owner := ctx.Value(httpsDialOwnerKey{}).(context.Context)
	dialCtx, cancel := context.WithCancelCause(owner)
	stop := context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
	return dialCtx, func() { stop(); cancel(nil) }
}

// Physical dialing belongs to this wrapper until the resulting connection is
// registered. Close cancels those exact calls as well as established sockets;
// request cancellation retains its own context and metadata throughout.
func (h *HTTPSTransportWrapper) dial(ctx context.Context, dialer N.Dialer, address M.Socksaddr, requireHTTP2 bool) (net.Conn, error) {
	var cancel context.CancelFunc
	if requireHTTP2 {
		ctx, cancel = context.WithCancel(ctx)
	} else {
		ctx, cancel = httpsHTTP1DialContext(ctx)
	}
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
