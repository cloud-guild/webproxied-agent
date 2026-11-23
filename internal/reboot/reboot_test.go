package reboot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloud-guild/webproxied-agent/internal/command"
)

// collector は、rpm の出力を差し替えた Collector を作る。ran に、実行したコマンドを記録する。
func collector(t *testing.T, root, rpmOut string, rpmErr error, ran *[]string) *Collector {
	t.Helper()
	return &Collector{
		Root: root,
		Run: func(_ context.Context, _ command.Options, name string, args ...string) ([]byte, error) {
			*ran = append(*ran, name+" "+strings.Join(args, " "))
			return []byte(rpmOut), rpmErr
		},
		LookPath: func(name string) (string, error) {
			if name == "rpm" {
				return "/usr/bin/rpm", nil
			}
			return "", command.ErrNotFound
		},
	}
}

func write(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func asJSON(st *Status) string {
	data, _ := json.Marshal(st)
	return string(data)
}

func TestDebian(t *testing.T) {
	root := t.TempDir()
	var ran []string
	c := collector(t, root, "", nil, &ran)

	if got := asJSON(c.Collect(context.Background(), "ubuntu", "debian")); got != `{"required":false,"method":"reboot-required","packages":[]}` {
		t.Errorf("印が無いとき: %s", got)
	}

	// 印があれば、パッケージの名前が読めなくても、再起動が要る
	write(t, root, "run/reboot-required", "*** System restart required ***\n")
	if got := asJSON(c.Collect(context.Background(), "debian", "")); got != `{"required":true,"method":"reboot-required","packages":[]}` {
		t.Errorf("名前が無いとき: %s", got)
	}

	write(t, root, "run/reboot-required.pkgs", "linux-image-6.8.0-49-generic\nlibc6\nlibc6\n"+strings.Repeat("x", 300)+"\n")
	got := c.Collect(context.Background(), "ubuntu", "debian")
	if *got.Required != true || len(*got.Packages) != 3 || (*got.Packages)[0] != "libc6" || len((*got.Packages)[2]) != maxName {
		t.Errorf("名前を整えない: %s", asJSON(got))
	}

	var names strings.Builder
	for i := 0; i < MaxPackages+5; i++ {
		fmt.Fprintf(&names, "pkg%03d\n", i)
	}
	write(t, root, "run/reboot-required.pkgs", names.String())
	if got := c.Collect(context.Background(), "ubuntu", "debian"); len(*got.Packages) != MaxPackages || !got.PackagesTruncated {
		t.Errorf("上限で切らない: %d %v", len(*got.Packages), got.PackagesTruncated)
	}
	if len(ran) != 0 {
		t.Errorf("Debian でコマンドを実行した: %v", ran)
	}
}

func TestRPM(t *testing.T) {
	root := t.TempDir()
	write(t, root, "proc/stat", "cpu  1 2 3\nbtime 1700000000\nprocesses 10\n")

	// 入っていないパッケージがあると、rpm は0でない状態で終わる（問題ではない）
	out := "kernel-core\t1690000000\nkernel-core\t1700000100\npackage kernel-rt is not installed\nglibc\t1699999999\nsystemd\t1700000001\n"
	var ran []string
	got := collector(t, root, out, errors.New("exit status 1"), &ran).Collect(context.Background(), "rocky", "rhel centos fedora")
	if s := asJSON(got); s != `{"required":true,"method":"rpm","packages":["kernel-core","systemd"]}` {
		t.Errorf("unexpected: %s", s)
	}
	if len(ran) != 1 || !strings.HasPrefix(ran[0], "rpm -q --qf %{NAME}\t%{INSTALLTIME}\n kernel ") {
		t.Errorf("決まっていないコマンドを実行した: %q", ran)
	}

	// 起動の後に入れたものが無い
	got = collector(t, root, "glibc\t1600000000\npackage kernel is not installed\n", errors.New("exit status 1"), &ran).Collect(context.Background(), "amzn", "centos rhel fedora")
	if s := asJSON(got); s != `{"required":false,"method":"rpm","packages":[]}` {
		t.Errorf("unexpected: %s", s)
	}
}

func TestErrors(t *testing.T) {
	root := t.TempDir()
	write(t, root, "proc/stat", "btime 1700000000\n")
	var ran []string

	cases := []struct {
		name, id, idLike, out, want string
		err                         error
		root                        string
	}{
		{name: "Alpine", id: "alpine", want: `{"error":"unsupported"}`},
		{name: "rpmのデータベースを開けない", id: "rhel", out: "error: rpmdb: BDB0113 Thread/process failed\n", err: errors.New("exit status 1"), want: `{"method":"rpm","error":"rpm_error"}`},
		{name: "rpmが実行できない", id: "rhel", err: errors.New("permission denied"), want: `{"method":"rpm","error":"rpm_error"}`},
		{name: "起動時刻が読めない", id: "rocky", idLike: "rhel", root: t.TempDir(), want: `{"method":"rpm","error":"read_error:/proc/stat"}`},
	}
	for _, c := range cases {
		r := root
		if c.root != "" {
			r = c.root
		}
		if got := asJSON(collector(t, r, c.out, c.err, &ran).Collect(context.Background(), c.id, c.idLike)); got != c.want {
			t.Errorf("%s: %s（期待 %s）", c.name, got, c.want)
		}
	}

	// rpm が入っていない
	c := collector(t, root, "", nil, &ran)
	c.LookPath = func(string) (string, error) { return "", command.ErrNotFound }
	if got := asJSON(c.Collect(context.Background(), "fedora", "")); got != `{"method":"rpm","error":"rpm_error"}` {
		t.Errorf("rpmが無い: %s", got)
	}
}
