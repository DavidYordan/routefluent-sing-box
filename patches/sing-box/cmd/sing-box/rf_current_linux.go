//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"syscall"

	"github.com/sagernet/sing-box/log"
)

var rfObserveCurrent bool

func init() {
	commandRun.Flags().BoolVar(&rfObserveCurrent, "rf-observe-current", false, "Expose loaded config to root on the process-owned abstract socket")
}

type rfRootPeer struct{}

// Called only after this instance's native Start succeeds. Failure is an
// observation error and never changes the already-running business instance.
func startRFCurrentObserve(raw []byte) func() {
	if !rfObserveCurrent {
		return func() {}
	}
	pid := os.Getpid()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: "@rf-business-current-" + strconv.Itoa(pid), Net: "unix"})
	if err != nil {
		log.Error(fmt.Errorf("native current observation: %w", err))
		return func() {}
	}
	content := bytes.Clone(raw)
	digest := sha256.Sum256(content)
	server := &http.Server{
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			peer, err := conn.(*net.UnixConn).SyscallConn()
			var credential *syscall.Ucred
			var credentialErr error
			if err == nil {
				err = peer.Control(func(fd uintptr) {
					credential, credentialErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
				})
			}
			root := err == nil && credentialErr == nil && credential != nil && credential.Uid == 0
			if !root {
				_ = conn.Close()
			}
			return context.WithValue(ctx, rfRootPeer{}, root)
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if root, _ := r.Context().Value(rfRootPeer{}).(bool); !root {
				http.Error(w, "root peer required", http.StatusForbidden)
				return
			}
			if r.URL.Path != "/v1/current" || r.URL.RawQuery != "" {
				http.NotFound(w, r)
				return
			}
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Rf-Native-Pid", strconv.Itoa(pid))
			w.Header().Set("X-Rf-Config-Sha256", hex.EncodeToString(digest[:]))
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			_, _ = w.Write(content)
		}),
	}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Error(fmt.Errorf("native current observation: %w", err))
		}
	}()
	return func() {
		if err := server.Close(); err != nil {
			log.Error(fmt.Errorf("close native current observation: %w", err))
		}
	}
}
