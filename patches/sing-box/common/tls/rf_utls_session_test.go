//go:build with_utls

package tls

import (
	"context"
	stdtls "crypto/tls"
	"testing"

	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/logger"
)

func TestRFUTLSChromeKeepsProfileAndResumesSupportedSessions(t *testing.T) {
	rfTestClientSession(t, func(ctx context.Context, log logger.ContextLogger, server string, options option.OutboundTLSOptions) (Config, error) {
		options.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: "chrome"}
		return NewClient(ctx, log, server, options)
	}, func(version uint16) bool {
		// The pinned Chrome preset has a TLS1.2 ticket extension but no
		// TLS1.3 PSK extension. A cache must not change that selected profile.
		return version == stdtls.VersionTLS12
	})
}

func TestRFUTLSRealityDoesNotAcquireOrdinarySessionCache(t *testing.T) {
	options := option.OutboundTLSOptions{
		Enabled: true, ServerName: "example.com",
		UTLS:    &option.OutboundUTLSOptions{Enabled: true, Fingerprint: "chrome"},
		Reality: &option.OutboundRealityOptions{Enabled: true},
	}
	config, err := newUTLSClient(t.Context(), logger.NOP(), "example.com", options, false)
	if err != nil {
		t.Fatal(err)
	}
	if config.(*UTLSClientConfig).config.ClientSessionCache != nil || config.Clone().(*UTLSClientConfig).config.ClientSessionCache != nil {
		t.Fatal("ordinary TLS cache entered the REALITY client's distinct authentication owner")
	}
}
