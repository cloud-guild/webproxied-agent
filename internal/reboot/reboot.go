// Package reboot は、再起動を待っている更新があるかを調べる。読み取りだけを行う。
//   - Debian・Ubuntu：/run/reboot-required（更新のパッケージが作る印）と、/run/reboot-required.pkgs。
//   - RHEL系・Amazon Linux：needs-restarting -r と同じ考え方で、中心のパッケージ（カーネル・glibc・systemd など）が、
//     起動の後に入ったかを、rpm の INSTALLTIME と /proc/stat の起動時刻で比べる
//     （needs-restarting は実行しない。dnf を root でなく動かすと、キャッシュの扱いや時間が読めず、入っていないサーバーもあるため）。
package reboot

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloud-guild/webproxied-agent/internal/command"
)

const (
	// MaxPackages は、送るパッケージの名前の数の上限。
	MaxPackages = 100
	// maxName は、パッケージの名前の長さの上限。
	maxName = 214
)

// corePackages は、更新したら再起動が要るパッケージ（dnf の needs-restarting -r と同じもの）。
var corePackages = []string{
	"kernel", "kernel-core", "kernel-rt", "kernel-rt-core", "kernel-64k", "kernel-64k-core",
	"glibc", "linux-firmware", "systemd", "udev", "openssl-libs", "gnutls",
	"dbus", "dbus-broker", "dbus-daemon", "microcode_ctl", "zlib",
}

// Status は、再起動を待っている更新。判断できなければ、Required の代わりに Error に理由を入れる。
type Status struct {
	Required *bool `json:"required,omitempty"`
	// Method は、調べ方（"reboot-required"・"rpm"）。
	Method string `json:"method,omitempty"`
	// Packages は、再起動を待っている更新のパッケージの名前（名前の順）。
	Packages          *[]string `json:"packages,omitempty"`
	PackagesTruncated bool      `json:"packages_truncated,omitempty"`
	Error             string    `json:"error,omitempty"`
}

// Collector は、再起動を待っている更新を調べる。
type Collector struct {
	// Root は、ファイルを読む場所（テストで差し替える）。
	Root string
	// Run は、コマンドの実行（テストで差し替える）。
	Run command.Runner
	// LookPath は、実行ファイルを探す（テストで差し替える）。
	LookPath func(name string) (string, error)
}

// Collect は、OS（/etc/os-release の ID・ID_LIKE）に合わせて調べる。
func (c *Collector) Collect(ctx context.Context, id, idLike string) *Status {
	words := strings.Fields(strings.ToLower(id + " " + idLike))
	has := func(names ...string) bool {
		for _, w := range words {
			for _, n := range names {
				if w == n {
					return true
				}
			}
		}
		return false
	}

	switch {
	case has("debian", "ubuntu"):
		return c.rebootRequired()
	case has("rhel", "centos", "fedora", "amzn"):
		if _, err := c.LookPath("rpm"); err != nil {
			return &Status{Method: "rpm", Error: "rpm_error"}
		}
		return c.rpm(ctx)
	}

	return &Status{Error: "unsupported"}
}

func (c *Collector) rebootRequired() *Status {
	const flag, pkgs = "/run/reboot-required", "/run/reboot-required.pkgs"
	st := &Status{Method: "reboot-required"}

	if _, err := os.Stat(filepath.Join(c.Root, flag)); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			st.Error = "read_error:" + flag
			return st
		}
		return result(st, false, nil)
	}

	// 印があれば、パッケージの名前が読めなくても、再起動が要る
	var names []string
	if f, err := os.Open(filepath.Join(c.Root, pkgs)); err == nil {
		data, _ := io.ReadAll(io.LimitReader(f, 1<<20))
		f.Close()
		names = strings.Fields(string(data))
	}

	return result(st, true, names)
}

var rpmOptions = command.Options{Timeout: 30 * time.Second, Limit: 1 << 20, Stderr: true}

func (c *Collector) rpm(ctx context.Context) *Status {
	st := &Status{Method: "rpm"}

	boot, ok := c.bootTime()
	if !ok {
		st.Error = "read_error:/proc/stat"
		return st
	}

	args := append([]string{"-q", "--qf", "%{NAME}\t%{INSTALLTIME}\n"}, corePackages...)
	out, err := c.Run(ctx, rpmOptions, "rpm", args...)
	if ctx.Err() != nil {
		st.Error = "rpm_error"
		return st
	}

	// 入っていないパッケージがあると、rpm は「package … is not installed」と出し、0でない状態で終わる（問題ではない）
	var names []string
	answered := false
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "error:"):
			// rpm のデータベースを開けないなど
			st.Error = "rpm_error"
			return st
		case strings.HasSuffix(line, " is not installed"):
			answered = true
		default:
			name, installed, ok := strings.Cut(line, "\t")
			t, err := strconv.ParseInt(installed, 10, 64)
			if !ok || err != nil {
				continue
			}
			answered = true
			if t > boot {
				names = append(names, name)
			}
		}
	}
	if err != nil && !answered {
		st.Error = "rpm_error"
		return st
	}

	return result(st, len(names) > 0, names)
}

// bootTime は、起動した時刻（/proc/stat の btime。UNIX時間の秒）を読む。
func (c *Collector) bootTime() (int64, bool) {
	data, err := os.ReadFile(filepath.Join(c.Root, "proc/stat"))
	if err != nil {
		return 0, false
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		if v, ok := strings.CutPrefix(scanner.Text(), "btime "); ok {
			t, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return t, err == nil && t > 0
		}
	}

	return 0, false
}

// result は、パッケージの名前を整えて（重複を除き、長すぎる名前を切り、名前の順にし、上限で切る）、結果にする。
func result(st *Status, required bool, names []string) *Status {
	seen := map[string]bool{}
	list := []string{}
	for _, n := range names {
		if len(n) > maxName {
			n = n[:maxName]
		}
		if n != "" && !seen[n] {
			seen[n] = true
			list = append(list, n)
		}
	}
	sort.Strings(list)
	if len(list) > MaxPackages {
		list, st.PackagesTruncated = list[:MaxPackages], true
	}

	st.Required, st.Packages = &required, &list
	return st
}
