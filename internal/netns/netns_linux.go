//go:build linux

package netns

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/vishvananda/netlink"
	vishnetns "github.com/vishvananda/netns"
)

// Namespace 描述一个已创建完成的网络命名空间。
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

// Create 创建命名空间、veth pair 与基础路由。
func Create(name string, index int) (*Namespace, error) {
	if index < 0 || index > 254 {
		return nil, fmt.Errorf("namespace index out of range: %d", index)
	}

	handle, err := vishnetns.NewNamed(name)
	if err != nil {
		return nil, fmt.Errorf("create namespace %s: %w", name, err)
	}

	namespace := &Namespace{
		Name:     name,
		Index:    index,
		Handle:   handle,
		VethHost: fmt.Sprintf("veth-%d", index),
		VethPeer: fmt.Sprintf("vpeer-%d", index),
		HostIP:   net.IPv4(10, 200, byte(index), 1).To4(),
		PeerIP:   net.IPv4(10, 200, byte(index), 2).To4(),
		Subnet:   fmt.Sprintf("10.200.%d.0/30", index),
	}

	if err := namespace.createVethPair(); err != nil {
		_ = namespace.Destroy()
		return nil, err
	}

	if err := namespace.configurePeer(); err != nil {
		_ = namespace.Destroy()
		return nil, err
	}

	if err := runCommand("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", namespace.Subnet, "-j", "MASQUERADE"); err != nil {
		_ = namespace.Destroy()
		return nil, fmt.Errorf("configure NAT for %s: %w", namespace.Subnet, err)
	}

	if err := writeNsResolvConf(name); err != nil {
		_ = namespace.Destroy()
		return nil, fmt.Errorf("write netns resolv.conf: %w", err)
	}

	return namespace, nil
}

func (ns *Namespace) createVethPair() error {
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: ns.VethHost},
		PeerName:  ns.VethPeer,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("create veth pair: %w", err)
	}

	hostLink, err := netlink.LinkByName(ns.VethHost)
	if err != nil {
		return fmt.Errorf("lookup host veth %s: %w", ns.VethHost, err)
	}

	hostAddr, err := netlink.ParseAddr(fmt.Sprintf("%s/30", ns.HostIP.String()))
	if err != nil {
		return fmt.Errorf("parse host addr: %w", err)
	}

	if err := netlink.AddrAdd(hostLink, hostAddr); err != nil {
		return fmt.Errorf("assign host addr: %w", err)
	}
	if err := netlink.LinkSetUp(hostLink); err != nil {
		return fmt.Errorf("bring up host veth: %w", err)
	}

	peerLink, err := netlink.LinkByName(ns.VethPeer)
	if err != nil {
		return fmt.Errorf("lookup peer veth %s: %w", ns.VethPeer, err)
	}
	if err := netlink.LinkSetNsFd(peerLink, int(ns.Handle)); err != nil {
		return fmt.Errorf("move peer into netns: %w", err)
	}

	return nil
}

func writeNsResolvConf(name string) error {
	dir := filepath.Join("/etc/netns", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	content := []byte("nameserver 1.1.1.1\nnameserver 1.0.0.1\n")
	return os.WriteFile(filepath.Join(dir, "resolv.conf"), content, 0o644)
}

func (ns *Namespace) configurePeer() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origin, err := vishnetns.Get()
	if err != nil {
		return fmt.Errorf("get current namespace: %w", err)
	}
	defer origin.Close()

	if err := vishnetns.Set(ns.Handle); err != nil {
		return fmt.Errorf("switch to namespace %s: %w", ns.Name, err)
	}
	defer func() {
		_ = vishnetns.Set(origin)
	}()

	loopback, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("lookup loopback: %w", err)
	}
	if err := netlink.LinkSetUp(loopback); err != nil {
		return fmt.Errorf("bring up loopback: %w", err)
	}

	peerLink, err := netlink.LinkByName(ns.VethPeer)
	if err != nil {
		return fmt.Errorf("lookup peer veth inside namespace: %w", err)
	}

	peerAddr, err := netlink.ParseAddr(fmt.Sprintf("%s/30", ns.PeerIP.String()))
	if err != nil {
		return fmt.Errorf("parse peer addr: %w", err)
	}
	if err := netlink.AddrAdd(peerLink, peerAddr); err != nil {
		return fmt.Errorf("assign peer addr: %w", err)
	}
	if err := netlink.LinkSetUp(peerLink); err != nil {
		return fmt.Errorf("bring up peer veth: %w", err)
	}

	defaultRoute := &netlink.Route{
		LinkIndex: peerLink.Attrs().Index,
		Gw:        ns.HostIP,
	}
	if err := netlink.RouteAdd(defaultRoute); err != nil {
		return fmt.Errorf("add default route: %w", err)
	}

	if err := applyKillSwitch(ns.VethPeer); err != nil {
		return fmt.Errorf("apply kill switch: %w", err)
	}

	return nil
}

func applyKillSwitch(vethPeer string) error {
	rules := [][]string{
		{"-F", "OUTPUT"},
		{"-P", "OUTPUT", "ACCEPT"},
		{"-A", "OUTPUT", "-o", "lo", "-j", "ACCEPT"},
		{"-A", "OUTPUT", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"-A", "OUTPUT", "-p", "udp", "-d", "1.1.1.1", "--dport", "53", "-j", "ACCEPT"},
		{"-A", "OUTPUT", "-p", "udp", "-d", "1.0.0.1", "--dport", "53", "-j", "ACCEPT"},
		{"-A", "OUTPUT", "-o", "tun+", "-j", "ACCEPT"},
		{"-A", "OUTPUT", "-o", vethPeer, "-j", "ACCEPT"},
		{"-P", "OUTPUT", "DROP"},
	}

	for _, rule := range rules {
		if err := runCommand("iptables", rule...); err != nil {
			return fmt.Errorf("iptables %v: %w", rule, err)
		}
	}

	return nil
}

// Destroy 清理命名空间相关资源。
func (ns *Namespace) Destroy() error {
	if ns == nil {
		return nil
	}

	if ns.Subnet != "" {
		_ = runCommand("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", ns.Subnet, "-j", "MASQUERADE")
	}

	if link, err := netlink.LinkByName(ns.VethHost); err == nil {
		_ = netlink.LinkDel(link)
	}

	_ = vishnetns.DeleteNamed(ns.Name)
	if ns.Handle.IsOpen() {
		_ = ns.Handle.Close()
	}
	_ = os.RemoveAll(filepath.Join("/etc/netns", ns.Name))

	return nil
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v failed: %w: %s", name, args, err, string(output))
	}

	return nil
}
