//go:build !linux

package netns

import (
	"errors"
	"net"

	vishnetns "github.com/vishvananda/netns"
)

// Namespace 是非 Linux 环境下的占位实现。
type Namespace struct {
	Name     string
	Index    int
	Handle   vishnetns.NsHandle
	VethHost string
	VethPeer string
	HostIP   net.IP
	PeerIP   net.IP
	Subnet   string
}

// Create 在非 Linux 平台上不可用。
func Create(name string, index int) (*Namespace, error) {
	_ = name
	_ = index
	return nil, errors.New("network namespaces require linux")
}

// Destroy 在非 Linux 平台上为空操作。
func (ns *Namespace) Destroy() error {
	_ = ns
	return nil
}
