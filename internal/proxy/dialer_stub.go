//go:build !linux

package proxy

import (
	"context"
	"errors"
	"net"

	vishnetns "github.com/vishvananda/netns"
)

// NsDialer 是非 Linux 环境下的占位实现。
type NsDialer struct{}

// DialInNs 在非 Linux 环境下不可用。
func (d *NsDialer) DialInNs(ctx context.Context, nsHandle vishnetns.NsHandle, network, address string) (net.Conn, error) {
	_ = d
	_ = ctx
	_ = nsHandle
	_ = network
	_ = address
	return nil, errors.New("namespace dialer requires linux")
}
