package main

import (
	"net"
	"time"
)

// NetworkChecker determines whether network connectivity is available.
type NetworkChecker interface {
	IsAvailable() bool
}

// RealNetworkChecker checks DNS connectivity by dialing 8.8.8.8:53.
type RealNetworkChecker struct{}

func (RealNetworkChecker) IsAvailable() bool {
	conn, err := net.DialTimeout("tcp", "8.8.8.8:53", 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// FakeNetworkChecker is a controllable network checker for tests.
type FakeNetworkChecker struct {
	Available bool
}

func (f FakeNetworkChecker) IsAvailable() bool {
	return f.Available
}
