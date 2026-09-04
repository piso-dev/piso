package server

import (
	"bufio"
	"net"
	"os"
	"strings"
)

// DenyPeer, when set, overrides vpc detection (tests).
// A true result means the control plane must answer 403.
type denyPeerFunc func(remoteAddr string) bool

func (s *Server) denyControlPeer(remoteAddr string) bool {
	if s != nil && s.DenyPeer != nil {
		return s.DenyPeer(remoteAddr)
	}
	return peerOnWorkerVPC(remoteAddr)
}

func parseRemoteIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return net.ParseIP(host)
}

type vpcNet struct {
	net  *net.IPNet
	self net.IP
	gw   net.IP
}

// peerOnWorkerVPC reports a peer that lives on the gateway's non-NAT
// (internal) interface. Those are workers. The bridge gateway and the
// gateway's own address are left through so host port-publish still works.
func peerOnWorkerVPC(remoteAddr string) bool {
	ip := parseRemoteIP(remoteAddr)
	if ip == nil || ip.IsLoopback() {
		return false
	}
	for _, n := range workerVPCNets() {
		if n.net == nil || !n.net.Contains(ip) {
			continue
		}
		if n.self != nil && ip.Equal(n.self) {
			return false
		}
		if n.gw != nil && ip.Equal(n.gw) {
			return false
		}
		return true
	}
	return false
}

func workerVPCNets() []vpcNet {
	def := defaultRouteIfaces()
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []vpcNet
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		if def[iface.Name] {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			four := ipn.IP.To4()
			out = append(out, vpcNet{net: ipn, self: four, gw: networkPlusOne(ipn)})
		}
	}
	return out
}

func defaultRouteIfaces() map[string]bool {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return map[string]bool{}
	}
	defer f.Close()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	if sc.Scan() {
		// header
	}
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		if fields[1] == "00000000" {
			out[fields[0]] = true
		}
	}
	return out
}

func networkPlusOne(n *net.IPNet) net.IP {
	ip := n.IP.To4()
	if ip == nil {
		return nil
	}
	mask := net.IP(n.Mask).To4()
	if mask == nil {
		return nil
	}
	out := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		out[i] = ip[i] & mask[i]
	}
	for i := 3; i >= 0; i-- {
		out[i]++
		if out[i] != 0 {
			break
		}
	}
	if !n.Contains(out) {
		return nil
	}
	return out
}
