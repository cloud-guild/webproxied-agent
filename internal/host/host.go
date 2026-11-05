// Package host は、サーバーの基本の情報（ホスト名・OS・カーネル・CPU・稼働時間）を集める。読み取りだけを行う。
package host

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Info は、サーバーの基本の情報。
type Info struct {
	Hostname      string `json:"hostname"`
	OSID          string `json:"os_id"`
	OSVersion     string `json:"os_version"`
	OSName        string `json:"os_name"`
	Kernel        string `json:"kernel"`
	Arch          string `json:"arch"`
	CPUs          int    `json:"cpus"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// Root は、/proc・/etcを読む場所（テストで差し替える）。
var Root = "/"

// Collect は、サーバーの基本の情報を集める。読めない項目は空のまま返す。
func Collect() Info {
	info := Info{Arch: runtime.GOARCH, CPUs: runtime.NumCPU()}
	info.Hostname, _ = os.Hostname()

	if release, err := os.Open(Root + "etc/os-release"); err == nil {
		values := ParseOSRelease(release)
		release.Close()
		info.OSID, info.OSVersion, info.OSName = values["ID"], values["VERSION_ID"], values["PRETTY_NAME"]
	}

	if kernel, err := os.ReadFile(Root + "proc/sys/kernel/osrelease"); err == nil {
		info.Kernel = strings.TrimSpace(string(kernel))
	}

	if uptime, err := os.ReadFile(Root + "proc/uptime"); err == nil {
		if fields := strings.Fields(string(uptime)); len(fields) > 0 {
			seconds, _ := strconv.ParseFloat(fields[0], 64)
			info.UptimeSeconds = int64(seconds)
		}
	}

	return info
}

// ParseOSRelease は、/etc/os-releaseの「KEY=VALUE」を読む。
func ParseOSRelease(r interface{ Read([]byte) (int, error) }) map[string]string {
	values := map[string]string{}
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if ok && !strings.HasPrefix(key, "#") {
			values[key] = strings.Trim(value, `"'`)
		}
	}

	return values
}
