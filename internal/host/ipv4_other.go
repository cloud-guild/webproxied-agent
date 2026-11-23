//go:build !linux

package host

import "errors"

// エージェントはLinuxだけに対応する（他のOSでは、IPv4のアドレスを送らない）
func ipv4Addresses() ([]ifaddr, error) {
	return nil, errors.New("このOSには対応していません")
}
