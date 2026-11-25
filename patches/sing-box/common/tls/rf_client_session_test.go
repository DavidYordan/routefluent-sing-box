package tls

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	stdtls "crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/logger"
)

type rfTLSCountConn struct {
	net.Conn
	read, written int
}

func (conn *rfTLSCountConn) Read(p []byte) (int, error) {
	n, err := conn.Conn.Read(p)
	conn.read += n
	return n, err
}

func (conn *rfTLSCountConn) Write(p []byte) (int, error) {
	n, err := conn.Conn.Write(p)
	conn.written += n
	return n, err
}

func TestRFSTDClientResumesWithinConfigAndKeepsTrustIsolated(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "example.com"}, DNSNames: []string{"example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	for _, version := range []struct {
		name string
		code uint16
	}{{"1.2", stdtls.VersionTLS12}, {"1.3", stdtls.VersionTLS13}} {
		t.Run(version.name, func(t *testing.T) {
			type result struct {
				resumed bool
				request string
			}
			results := make(chan result, 3)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				results <- result{resumed: r.TLS.DidResume, request: r.Header.Get("X-Request")}
				_, _ = io.WriteString(w, "ok")
			}))
			server.TLS = &stdtls.Config{MinVersion: version.code, MaxVersion: version.code, Certificates: []stdtls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
			server.StartTLS()
			defer server.Close()
			newClient := func(trusted bool) Config {
				options := option.OutboundTLSOptions{Enabled: true, ServerName: "example.com", MinVersion: version.name, MaxVersion: version.name}
				if trusted {
					options.Certificate = []string{certificatePEM}
				}
				config, err := NewSTDClient(t.Context(), logger.NOP(), "example.com", options)
				if err != nil {
					t.Fatal(err)
				}
				return config
			}
			client := newClient(true)
			var costs [3]int
			for index := range costs {
				config := client
				if index == 1 {
					config = client.Clone()
				} else if index == 2 {
					config = newClient(true)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				counted := &rfTLSCountConn{Conn: raw}
				conn, err := ClientHandshake(ctx, counted, config)
				if err != nil {
					_ = raw.Close()
					cancel()
					t.Fatal(err)
				}
				requestID := fmt.Sprintf("request-%d", index)
				_, err = fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: example.com\r\nX-Request: %s\r\nConnection: close\r\n\r\n", requestID)
				if err != nil {
					t.Fatal(err)
				}
				response, err := http.ReadResponse(bufio.NewReader(conn), nil)
				if err != nil {
					t.Fatal(err)
				}
				body, readErr := io.ReadAll(response.Body)
				closeErr := response.Body.Close()
				costs[index] = counted.read + counted.written
				_ = conn.Close()
				cancel()
				if readErr != nil || closeErr != nil || string(body) != "ok" {
					t.Fatalf("response changed: %q, %v, %v", body, readErr, closeErr)
				}
				observed := <-results
				if observed.resumed != (index == 1) || observed.request != requestID {
					t.Errorf("request %d: resumed=%t, request=%s", index, observed.resumed, observed.request)
				}
			}
			if costs[1] >= costs[0] {
				t.Errorf("resumed connection repeated full TLS bytes: full=%d resumed=%d", costs[0], costs[1])
			}
			t.Logf("RF_NATIVE_TLS version=%s full=%d reused=%d independent=%d; TLS+HTTP socket bytes, excludes IP/ACK/proxy and daily coverage", version.name, costs[0], costs[1], costs[2])

			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			_, err = ClientHandshake(ctx, raw, newClient(false))
			var unknown x509.UnknownAuthorityError
			if !errors.As(err, &unknown) {
				t.Fatalf("another TLS config borrowed the trusted session: %v", err)
			}
		})
	}
}
