package host

import (
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOSRelease(t *testing.T) {
	values := ParseOSRelease(strings.NewReader("NAME=\"Ubuntu\"\nVERSION_ID=\"24.04\"\nID=ubuntu\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\n"))
	if values["ID"] != "ubuntu" || values["VERSION_ID"] != "24.04" || values["PRETTY_NAME"] != "Ubuntu 24.04.1 LTS" {
		t.Fatalf("unexpected: %v", values)
	}
}

func TestCollect(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) {
		full := filepath.Join(root, path)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o644)
	}
	write("etc/os-release", "ID=almalinux\nVERSION_ID=\"9.8\"\nPRETTY_NAME=\"AlmaLinux 9.8\"\n")
	write("proc/sys/kernel/osrelease", "5.14.0\n")
	write("proc/uptime", "3600.50 100.00\n")

	Root = root + "/"
	defer func() { Root = "/" }()

	info := Collect()
	if info.OSID != "almalinux" || info.OSVersion != "9.8" || info.Kernel != "5.14.0" || info.UptimeSeconds != 3600 {
		t.Fatalf("unexpected: %+v", info)
	}
}

func TestCollectIPAddresses(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) {
		full := filepath.Join(root, path)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o644)
	}
	// IPv6：global・ULA・link-local・ループバック・一時的・確認中・非推奨・docker
	write("proc/net/if_inet6", strings.Join([]string{
		"2406da1408e2b30012345678abcdef01 02 40 00 80     eth0",
		"fd000000000000000000000000000001 02 40 00 80     eth0",
		"fe80000000000000000000fffe000001 02 40 20 80     eth0",
		"00000000000000000000000000000001 01 80 10 80       lo",
		"2406da1408e2b300aaaaaaaaaaaaaaaa 02 40 00 01     eth0",
		"2406da1408e2b300bbbbbbbbbbbbbbbb 02 40 00 40     eth0",
		"2406da1408e2b300cccccccccccccccc 02 40 00 20     eth0",
		"20010db8000000000000000000000002 05 40 00 80  docker0",
		"20010db8000000000000000000000003 06 40 00 80     eth1",
		"bad line",
	}, "\n")+"\n")
	write("sys/class/net/eth0/flags", "0x1003\n")
	write("sys/class/net/eth1/flags", "0x1002\n") // down
	write("sys/class/net/ens5/flags", "0x1003\n")

	Root = root + "/"
	defer func() { Root = "/"; ipv4Source = ipv4Addresses }()
	ipv4Source = func() ([]ifaddr, error) {
		return []ifaddr{
			{name: "lo", addr: netip.MustParseAddr("127.0.0.1"), prefix: 8},
			{name: "eth0", addr: netip.MustParseAddr("10.0.1.12"), prefix: 24},
			{name: "eth0:1", addr: netip.MustParseAddr("203.0.113.5"), prefix: 32},
			{name: "eth0", addr: netip.MustParseAddr("169.254.10.1"), prefix: 16},
			{name: "docker0", addr: netip.MustParseAddr("172.17.0.1"), prefix: 16},
			{name: "veth1234", addr: netip.MustParseAddr("10.10.0.1"), prefix: 24},
			{name: "eth1", addr: netip.MustParseAddr("192.168.0.5"), prefix: 24},
			{name: "ens5", addr: netip.MustParseAddr("100.64.0.9"), prefix: 10},
			{name: "ens5", addr: netip.MustParseAddr("100.64.0.9"), prefix: 10},
		}, nil
	}

	data, _ := json.Marshal(CollectIPAddresses())
	want := `[{"interface":"ens5","address":"100.64.0.9","prefix":10},` +
		`{"interface":"eth0","address":"10.0.1.12","prefix":24},{"interface":"eth0","address":"203.0.113.5","prefix":32},` +
		`{"interface":"eth0","address":"2406:da14:8e2:b300:1234:5678:abcd:ef01","prefix":64},{"interface":"eth0","address":"fd00::1","prefix":64}]`
	if string(data) != want {
		t.Errorf("got  %s\nwant %s", data, want)
	}

	// どちらも集められなければ、送らない
	os.Remove(filepath.Join(root, "proc/net/if_inet6"))
	ipv4Source = func() ([]ifaddr, error) { return nil, errors.New("x") }
	if got := CollectIPAddresses(); got != nil {
		t.Errorf("nil でない: %v", got)
	}
	info := Info{IPAddresses: CollectIPAddresses()}
	if data, _ := json.Marshal(info); strings.Contains(string(data), "ip_addresses") {
		t.Errorf("キーを送った: %s", data)
	}

	// 上限
	var many []ifaddr
	for i := 0; i < MaxIPAddresses+10; i++ {
		many = append(many, ifaddr{name: "eth0", addr: netip.AddrFrom4([4]byte{10, 0, 0, byte(i + 1)}), prefix: 24})
	}
	ipv4Source = func() ([]ifaddr, error) { return many, nil }
	if got := CollectIPAddresses(); len(got) != MaxIPAddresses || got[0].Address != "10.0.0.1" {
		t.Errorf("上限で切らない: %d %v", len(got), got[0])
	}
}

// TestIPv4Real は、このマシンで ioctl が動き、ループバックのアドレスを返すことを確かめる（除く前の値）。
func TestIPv4Real(t *testing.T) {
	list, err := ipv4Addresses()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		if a.name == "lo" && a.addr == netip.MustParseAddr("127.0.0.1") && a.prefix == 8 {
			return
		}
	}
	t.Errorf("lo の 127.0.0.1/8 が無い: %v", list)
}

// TestRoutePrefix は、同じ名前の2つ目以降のIPv4のアドレスの、プレフィックス長の求め方を確かめる（リトルエンディアンのマシン）。
func TestRoutePrefix(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "proc/net"), 0o755)
	os.WriteFile(filepath.Join(root, "proc/net/route"), []byte(
		"Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
			"eth0\t00000000\t010011AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"+ // 既定の経路（ゲートウェイあり）
			"eth0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"+ // 172.17.0.0/16
			"eth0\t006433C6\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"+ // 198.51.100.0/24
			"eth1\t006433C6\t00000000\t0001\t0\t0\t0\t80FFFFFF\t0\t0\t0\n"), 0o644) // 198.51.100.0/25（別のインターフェース）
	Root = root + "/"
	defer func() { Root = "/" }()

	routes := readRoutes()
	cases := map[string]int{"198.51.100.7": 24, "172.17.0.3": 16, "203.0.113.9": 32}
	for addr, want := range cases {
		if got := routePrefix(routes, "eth0", netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %d（期待 %d）", addr, got, want)
		}
	}
	if got := routePrefix(routes, "eth1", netip.MustParseAddr("198.51.100.7")); got != 25 {
		t.Errorf("eth1: %d", got)
	}
}
