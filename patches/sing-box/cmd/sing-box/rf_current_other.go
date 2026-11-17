//go:build !linux

package main

import (
	"context"
	"github.com/sagernet/sing-box/common/rfconnection"
)

func rfCurrentContext(ctx context.Context) context.Context { return ctx }

func startRFCurrentObserve([]byte, func() []rfconnection.Connection) func() { return func() {} }
