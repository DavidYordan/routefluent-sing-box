//go:build !linux

package main

func startRFCurrentObserve([]byte) func() { return func() {} }
