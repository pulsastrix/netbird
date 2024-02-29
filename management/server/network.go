package server

import (
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/c-robinson/iplib"
	"github.com/rs/xid"

	nbdns "github.com/netbirdio/netbird/dns"
	nbpeer "github.com/netbirdio/netbird/management/server/peer"
	"github.com/netbirdio/netbird/management/server/status"
	"github.com/netbirdio/netbird/route"
)

const (
	// SubnetSize is a size of the subnet of the global network, e.g.  100.77.0.0/16
	SubnetSize = 16
	// NetSize is a global network size 100.64.0.0/10
	NetSize = 10
	// Subnet6Size is the size of an IPv6 subnet (in Bytes, not Bits)
	Subnet6Size = 8

	// AllowedIPsFormat generates Wireguard AllowedIPs format (e.g. 100.64.30.1/32)
	AllowedIPsFormat = "%s/32"

	// AllowedIP6sFormat generates Wireguard AllowedIPs format (e.g. 2001:db8::dead:beef/128)
	AllowedIP6sFormat = "%s/128"
)

type NetworkMap struct {
	Peers         []*nbpeer.Peer
	Network       *Network
	Routes        []*route.Route
	DNSConfig     nbdns.Config
	OfflinePeers  []*nbpeer.Peer
	FirewallRules []*FirewallRule
}

type Network struct {
	Identifier string     `json:"id"`
	Net        net.IPNet  `gorm:"serializer:gob"`
	Net6       *net.IPNet `gorm:"serializer:gob"`
	Dns        string
	// Serial is an ID that increments by 1 when any change to the network happened (e.g. new peer has been added).
	// Used to synchronize state to the client apps.
	Serial uint64

	mu sync.Mutex `json:"-" gorm:"-"`
}

// NewNetwork creates a new Network initializing it with a Serial=0
// It takes a random /16 subnet from 100.64.0.0/10 (64 different subnets)
func NewNetwork(enableV6 bool) *Network {

	n := iplib.NewNet4(net.ParseIP("100.64.0.0"), NetSize)
	sub, _ := n.Subnet(SubnetSize)

	s := rand.NewSource(time.Now().Unix())
	r := rand.New(s)
	intn := r.Intn(len(sub))

	var n6 *net.IPNet = nil
	if enableV6 {
		n6 = GenerateNetwork6()
	}

	return &Network{
		Identifier: xid.New().String(),
		Net:        sub[intn].IPNet,
		Net6:       n6,
		Dns:        "",
		Serial:     0}
}

func GenerateNetwork6() *net.IPNet {
	s := rand.NewSource(time.Now().Unix())
	r := rand.New(s)
	addrbuf := make([]byte, 16)
	addrbuf[0] = 0xfd
	addrbuf[1] = 0x00
	addrbuf[2] = 0xb1
	addrbuf[3] = 0x4d
	_, _ = r.Read(addrbuf[4:Subnet6Size])

	n6 := iplib.NewNet6(addrbuf, Subnet6Size*8, 0).IPNet
	return &n6
}

// IncSerial increments Serial by 1 reflecting that the network state has been changed
func (n *Network) IncSerial() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Serial++
}

// CurrentSerial returns the Network.Serial of the network (latest state id)
func (n *Network) CurrentSerial() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.Serial
}

func (n *Network) Copy() *Network {
	return &Network{
		Identifier: n.Identifier,
		Net:        n.Net,
		Net6:       n.Net6,
		Dns:        n.Dns,
		Serial:     n.Serial,
	}
}

// AllocatePeerIP pics an available IP from an net.IPNet.
// This method considers already taken IPs and reuses IPs if there are gaps in takenIps
// E.g. if ipNet=100.30.0.0/16 and takenIps=[100.30.0.1, 100.30.0.4] then the result would be 100.30.0.2 or 100.30.0.3
func AllocatePeerIP(ipNet net.IPNet, takenIps []net.IP) (net.IP, error) {
	takenIPMap := make(map[string]struct{})
	takenIPMap[ipNet.IP.String()] = struct{}{}
	for _, ip := range takenIps {
		takenIPMap[ip.String()] = struct{}{}
	}

	ips, _ := generateIPs(&ipNet, takenIPMap)

	if len(ips) == 0 {
		return nil, status.Errorf(status.PreconditionFailed, "failed allocating new IP for the ipNet %s - network is out of IPs", ipNet.String())
	}

	// pick a random IP
	s := rand.NewSource(time.Now().Unix())
	r := rand.New(s)
	intn := r.Intn(len(ips))

	return ips[intn], nil
}

// AllocatePeerIP6 pics an available IPv6 from an net.IPNet.
// This method considers already taken IPs and reuses IPs if there are gaps in takenIps.
func AllocatePeerIP6(ipNet net.IPNet, takenIps []net.IP) (net.IP, error) {

	takenIPMap := make(map[string]struct{})
	takenIPMap[ipNet.IP.String()] = struct{}{}
	for _, ip := range takenIps {
		takenIPMap[ip.String()] = struct{}{}
	}

	maskSize, _ := ipNet.Mask.Size()

	s := rand.NewSource(time.Now().Unix())
	r := rand.New(s)

	// TODO for small subnet sizes, randomly generating values until we don't get a duplicate is inefficient and could
	// 		lead to many loop iterations, using a method similar to IPv4 would be preferable here.

	addrbuf := make(net.IP, 16)
	copy(addrbuf, ipNet.IP.To16())
	for duplicate := true; duplicate; _, duplicate = takenIPMap[addrbuf.String()] {
		_, _ = r.Read(addrbuf[(maskSize / 8):16])
	}
	return addrbuf, nil
}

// generateIPs generates a list of all possible IPs of the given network excluding IPs specified in the exclusion list
func generateIPs(ipNet *net.IPNet, exclusions map[string]struct{}) ([]net.IP, int) {

	var ips []net.IP
	for ip := ipNet.IP.Mask(ipNet.Mask); ipNet.Contains(ip); incIP(ip) {
		if _, ok := exclusions[ip.String()]; !ok && ip[3] != 0 {
			ips = append(ips, copyIP(ip))
		}
	}

	// remove network address, broadcast and Fake DNS resolver address
	lenIPs := len(ips)
	switch {
	case lenIPs < 2:
		return ips, lenIPs
	case lenIPs < 3:
		return ips[1 : len(ips)-1], lenIPs - 2
	default:
		return ips[1 : len(ips)-2], lenIPs - 3
	}
}

func copyIP(ip net.IP) net.IP {
	dup := make(net.IP, len(ip))
	copy(dup, ip)
	return dup
}

func incIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}
