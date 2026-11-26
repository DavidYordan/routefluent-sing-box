package transport

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mDNS "github.com/miekg/dns"
)

func rfHTTPSMaximumReply(t *testing.T, request *mDNS.Msg) []byte {
	t.Helper()
	response := new(mDNS.Msg)
	response.SetReply(request)
	padding := &mDNS.EDNS0_PADDING{}
	response.Extra = []mDNS.RR{&mDNS.OPT{Hdr: mDNS.RR_Header{Name: ".", Rrtype: mDNS.TypeOPT, Class: 4096}, Option: []mDNS.EDNS0{padding}}}
	raw, err := response.Pack()
	if err != nil {
		t.Fatal(err)
	}
	padding.Padding = make([]byte, 65535-len(raw))
	raw, err = response.Pack()
	if err != nil || len(raw) != 65535 {
		t.Fatal("maximum legal DoH fixture differs", len(raw), err)
	}
	return raw
}

func TestRFHTTPSWireLimitPreservesSiblingAndMaximumReply(t *testing.T) {
	for _, declared := range []bool{true, false} {
		t.Run(fmt.Sprintf("declared=%t", declared), func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			releaseHealthy := func() { once.Do(func() { close(release) }) }
			doh, connections := rfHTTPSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request := rfHTTPSQuestion(t, r)
				if request == nil {
					return
				}
				if request.Question[0].Name == "healthy.example." {
					close(started)
					<-release
					rfHTTPSReply(t, w, request)
					return
				}
				raw := rfHTTPSMaximumReply(t, request)
				if request.Question[0].Name == "oversize.example." {
					// The DNS decoder accepts this valid prefix plus a trailing
					// byte. The enclosing DoH media type must reject its size.
					raw = append(raw, 0)
				}
				w.Header().Set("Content-Type", MimeType)
				if declared {
					w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
				} else {
					w.(http.Flusher).Flush()
				}
				_, _ = w.Write(raw)
			}))
			t.Cleanup(releaseHealthy)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			query := func(name string) *mDNS.Msg { q := new(mDNS.Msg); q.SetQuestion(name, mDNS.TypeA); return q }
			healthy := make(chan error, 1)
			go func() { _, err := doh.Exchange(ctx, query("healthy.example.")); healthy <- err }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("healthy stream did not start")
			}
			if answer, err := doh.Exchange(ctx, query("oversize.example.")); err == nil || answer != nil || !strings.Contains(err.Error(), "wire limit") {
				t.Errorf("oversize DoH was not rejected at its message boundary: answer=%t error=%v", answer != nil, err)
			}
			releaseHealthy()
			if err := <-healthy; err != nil {
				t.Fatal("oversize response closed its healthy sibling", err)
			}
			answer, err := doh.Exchange(ctx, query("maximum.example."))
			if err != nil || answer == nil || len(answer.Extra) != 1 {
				t.Fatal("maximum legal DNS message was rejected", err)
			}
			if connections.opened.Load() != 1 {
				t.Fatal("message rejection replaced the shared TLS connection", connections.opened.Load())
			}
		})
	}
}

func TestRFHTTPSRejectsDeclaredOversizeBeforeBodyRead(t *testing.T) {
	doh, _ := rfHTTPSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rfHTTPSQuestion(t, r) == nil {
			return
		}
		w.Header().Set("Content-Type", MimeType)
		w.Header().Set("Content-Length", "1048576")
		w.WriteHeader(http.StatusOK)
		// No body is sent. A bounded decoder rejects the declaration rather
		// than allocating for it and waiting for the promised bytes.
	}))
	request := new(mDNS.Msg)
	request.SetQuestion("declared.example.", mDNS.TypeA)
	_, err := doh.Exchange(t.Context(), request)
	if err == nil || !strings.Contains(err.Error(), "wire limit") {
		t.Fatal("oversize declaration was read as a response body", err)
	}
}
