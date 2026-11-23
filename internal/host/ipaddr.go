package host

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
)

// MaxIPAddresses は、送るIPアドレスの数の上限。
const MaxIPAddresses = 32

// IPAddress は、インターフェースに付いたIPアドレス。パブリック・プライベートの判断は、アプリが行う。
type IPAddress struct {
	Interface string `json:"interface"`
	// Address は、正規化したアドレス（IPv6は短縮形）。
	Address string `json:"address"`
	Prefix  int    `json:"prefix"`
}

// virtualPrefixes は、コンテナ・仮想ネットワーク・VPNのインターフェースの名前の始まり（このサーバーのアドレスではないため、送らない）。
var virtualPrefixes = []string{
	"lo", "docker", "br-", "veth", "virbr", "cni", "flannel", "cali", "vxlan", "tun", "tap", "wg",
	"kube-", "cilium", "lxc", "lxd", "podman", "weave", "dummy", "nerdctl", "tailscale", "zt",
}

// ipv4Source は、IPv4のアドレスを集める（テストで差し替える）。
var ipv4Source = ipv4Addresses

// ifaddr は、集めたアドレス（名前・アドレス・プレフィックス長）。
type ifaddr struct {
	name   string
	addr   netip.Addr
	prefix int
}

// CollectIPAddresses は、up のインターフェースのIPv4・IPv6のアドレスを集める。
// ループバック・リンクローカル・仮想のインターフェースは除く。集められなければ nil を返す。
//
// netlink（Go の net.Interfaces）は使わない。サービスは RestrictAddressFamilies で AF_NETLINK を使えないため、
// IPv4は AF_INET のソケットの ioctl（SIOCGIFCONF）で、IPv6は /proc/net/if_inet6 で読む。
func CollectIPAddresses() []IPAddress {
	var found []ifaddr
	ok := false

	if v4, err := ipv4Source(); err == nil {
		ok = true
		found = append(found, v4...)
	}
	if v6, err := ipv6Addresses(); err == nil {
		ok = true
		found = append(found, v6...)
	}
	if !ok {
		return nil
	}

	seen := map[string]bool{}
	list := []IPAddress{}
	for _, a := range found {
		// ラベル（eth0:1）は、インターフェースの名前にする
		name, _, _ := strings.Cut(a.name, ":")
		if name == "" || virtual(name) || !up(name) || !a.addr.IsValid() {
			continue
		}
		addr := a.addr.Unmap()
		if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsMulticast() || addr.IsUnspecified() {
			continue
		}
		key := name + " " + addr.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		list = append(list, IPAddress{Interface: name, Address: addr.String(), Prefix: a.prefix})
	}

	// インターフェースの名前の順、IPv4を先に、アドレスの順
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Interface != b.Interface {
			return a.Interface < b.Interface
		}
		a4, b4 := !strings.Contains(a.Address, ":"), !strings.Contains(b.Address, ":")
		if a4 != b4 {
			return a4
		}
		return netip.MustParseAddr(a.Address).Less(netip.MustParseAddr(b.Address))
	})
	if len(list) > MaxIPAddresses {
		list = list[:MaxIPAddresses]
	}

	return list
}

func virtual(name string) bool {
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// up は、インターフェースが up か（/sys/class/net/<名前>/flags の IFF_UP）。読めなければ、up とみなす。
func up(name string) bool {
	if strings.ContainsAny(name, "/\x00") || name == "." || name == ".." {
		return false
	}
	data, err := os.ReadFile(Root + "sys/class/net/" + name + "/flags")
	if err != nil {
		return true
	}
	flags, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(string(data)), "0x"), 16, 32)
	return err != nil || flags&0x1 != 0
}

// route は、/proc/net/route の、ゲートウェイの無い（直接つながる）経路。
type route struct {
	iface  string
	prefix netip.Prefix
}

// readRoutes は、/proc/net/route を読む（アドレスは、カーネルの中の並びのまま16進で書かれている）。
func readRoutes() []route {
	data, err := os.ReadFile(Root + "proc/net/route")
	if err != nil {
		return []route{}
	}

	list := []route{}
	for _, line := range strings.Split(string(data), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 8 || f[2] != "00000000" {
			continue
		}
		dest, err1 := hexAddr4(f[1])
		mask, err2 := hexAddr4(f[7])
		if err1 != nil || err2 != nil {
			continue
		}
		bitsLen := 0
		for _, b := range mask.As4() {
			for ; b&0x80 != 0; b <<= 1 {
				bitsLen++
			}
		}
		if bitsLen == 0 {
			continue
		}
		list = append(list, route{iface: f[0], prefix: netip.PrefixFrom(dest, bitsLen)})
	}

	return list
}

// hexAddr4 は、/proc/net/route の16進（メモリの並びのまま）を、IPv4のアドレスにする。
func hexAddr4(s string) (netip.Addr, error) {
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return netip.Addr{}, err
	}
	var b [4]byte
	binary.NativeEndian.PutUint32(b[:], uint32(v))
	return netip.AddrFrom4(b), nil
}

// routePrefix は、そのインターフェースの経路のうち、アドレスを含む最も長いもののプレフィックス長。無ければ32。
func routePrefix(routes []route, iface string, addr netip.Addr) int {
	best := 32
	found := false
	for _, r := range routes {
		if r.iface == iface && r.prefix.Contains(addr) && (!found || r.prefix.Bits() > best) {
			best, found = r.prefix.Bits(), true
		}
	}
	return best
}

// IPv6のアドレスの状態（/proc/net/if_inet6 の flags）。一時的・重複・確認中・非推奨のアドレスは送らない。
const (
	ifaFTemporary  = 0x01
	ifaFDadFailed  = 0x08
	ifaFDeprecated = 0x20
	ifaFTentative  = 0x40
)

// ipv6Addresses は、/proc/net/if_inet6 を読む（アドレス・番号・プレフィックス長・スコープ・状態・名前）。
func ipv6Addresses() ([]ifaddr, error) {
	f, err := os.Open(Root + "proc/net/if_inet6")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var list []ifaddr
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 6 || len(fields[0]) != 32 {
			continue
		}
		raw, err := hex.DecodeString(fields[0])
		prefix, err2 := strconv.ParseUint(fields[2], 16, 8)
		scope, err3 := strconv.ParseUint(fields[3], 16, 8)
		flags, err4 := strconv.ParseUint(fields[4], 16, 32)
		if err != nil || err2 != nil || err3 != nil || err4 != nil || prefix > 128 {
			continue
		}
		// スコープは global（0）だけ。状態の悪いものは除く
		if scope != 0 || flags&(ifaFTemporary|ifaFDadFailed|ifaFDeprecated|ifaFTentative) != 0 {
			continue
		}
		list = append(list, ifaddr{name: fields[5], addr: netip.AddrFrom16([16]byte(raw)), prefix: int(prefix)})
	}

	return list, scanner.Err()
}
