//go:build !linux

package main

import (
	"context"
	"github.com/sagernet/sing-box/common/rfconnection"
	"github.com/sagernet/sing-box/common/trafficcontrol"
)

func rfCurrentContext(ctx context.Context) context.Context { return ctx }

func startRFCurrentObserve([]byte, func() []rfconnection.Connection, func() (trafficcontrol.RFActivity, error)) func() {
	return func() {}
}
