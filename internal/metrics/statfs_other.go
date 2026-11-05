//go:build !linux

package metrics

import "errors"

// エージェントはLinuxだけに対応する（他のOSでは、ディスクの容量を送らない）
func statfs(string) (FSStat, error) {
	return FSStat{}, errors.New("このOSには対応していません")
}
