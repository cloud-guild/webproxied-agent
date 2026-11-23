package host

import (
	"bytes"
	"encoding/binary"
	"math/bits"
	"net/netip"
	"runtime"
	"syscall"
	"unsafe"
)

// ifreqSize は、struct ifreq の大きさ（名前16バイトと、24バイトの共用体。64ビットのLinux）。
const ifreqSize = 40

// maxInterfaces は、SIOCGIFCONF で受け取るアドレスの数の上限。
const maxInterfaces = 256

// ipv4Addresses は、AF_INET のソケットの ioctl（SIOCGIFCONF・SIOCGIFNETMASK）で、IPv4のアドレスを集める。
func ipv4Addresses() ([]ifaddr, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)

	buf := make([]byte, ifreqSize*maxInterfaces)
	ifc := struct {
		Len int32
		_   [4]byte
		Buf uintptr
	}{Len: int32(len(buf)), Buf: uintptr(unsafe.Pointer(&buf[0]))}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCGIFCONF, uintptr(unsafe.Pointer(&ifc)))
	runtime.KeepAlive(buf)
	if errno != 0 {
		return nil, errno
	}

	var list []ifaddr
	var routes []route
	seen := map[string]bool{}
	for off := 0; off+ifreqSize <= int(ifc.Len); off += ifreqSize {
		req := buf[off : off+ifreqSize]
		// sockaddr_in：family（2バイト）・port（2バイト）・アドレス（4バイト）
		if binary.NativeEndian.Uint16(req[16:18]) != syscall.AF_INET {
			continue
		}
		name := req[:16]
		if i := bytes.IndexByte(name, 0); i >= 0 {
			name = name[:i]
		}
		addr := netip.AddrFrom4([4]byte(req[20:24]))

		// SIOCGIFNETMASK は、名前（ラベル）の最初のアドレスのマスクを返す。
		// 同じ名前の2つ目以降（ip addr add で足したもの）は、経路（/proc/net/route）から求める
		prefix := 32
		if !seen[string(name)] {
			seen[string(name)] = true
			var mask [ifreqSize]byte
			copy(mask[:16], req[:16])
			if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCGIFNETMASK, uintptr(unsafe.Pointer(&mask))); errno == 0 {
				prefix = bits.OnesCount32(binary.BigEndian.Uint32(mask[20:24]))
			}
		} else {
			if routes == nil {
				routes = readRoutes()
			}
			prefix = routePrefix(routes, string(name), addr)
		}

		list = append(list, ifaddr{name: string(name), addr: addr, prefix: prefix})
	}

	return list, nil
}
