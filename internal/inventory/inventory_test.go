package inventory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cloud-guild/webproxied-agent/internal/command"
)

func TestParseDpkg(t *testing.T) {
	out := "ii \tnginx\t1.24.0-2ubuntu7\tnginx\t1.24.0-2ubuntu7\tamd64\n" +
		"rc \told\t1.0\told\t1.0\tall\n" +
		"ii \tlibc6:amd64\t2.39-0ubuntu8\tglibc\t2.39-0ubuntu8\tamd64\n" +
		"ii \tlibfoo1\t1.2+b1\t\t\tarm64\n"
	want := []Package{
		{"nginx", "1.24.0-2ubuntu7", "amd64", "nginx", "1.24.0-2ubuntu7"},
		{"libc6", "2.39-0ubuntu8", "amd64", "glibc", "2.39-0ubuntu8"},
		{"libfoo1", "1.2+b1", "arm64", "libfoo1", "1.2+b1"},
	}
	if got := ParseDpkg([]byte(out)); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestParseRPM(t *testing.T) {
	out := "openssl-libs\t1:3.0.7-27.el9\tx86_64\topenssl-3.0.7-27.el9.src.rpm\n" +
		"gpg-pubkey\t0:3228467c-613798eb\t(none)\t(none)\n" +
		"bash\t0:5.1.8-9.el9\tx86_64\tbash-5.1.8-9.el9.src.rpm\n" +
		"odd\t0:1-1\tnoarch\t(none)\n"
	want := []Package{
		{"openssl-libs", "1:3.0.7-27.el9", "x86_64", "openssl", "1:3.0.7-27.el9"},
		{"bash", "5.1.8-9.el9", "x86_64", "bash", "5.1.8-9.el9"},
		{"odd", "1-1", "noarch", "odd", "1-1"},
	}
	if got := ParseRPM([]byte(out)); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestParseAPK(t *testing.T) {
	db := "C:Q1abc\nP:musl\nV:1.2.5-r0\nA:x86_64\nS:1\no:musl\n\nP:ssl_client\nV:1.36.1-r29\nA:x86_64\no:busybox\n\nP:noorigin\nV:1-r0\nA:x86_64\n"
	want := []Package{
		{"musl", "1.2.5-r0", "x86_64", "musl", "1.2.5-r0"},
		{"ssl_client", "1.36.1-r29", "x86_64", "busybox", "1.36.1-r29"},
		{"noorigin", "1-r0", "x86_64", "noorigin", "1-r0"},
	}
	if got := ParseAPK(strings.NewReader(db)); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestParseUnits(t *testing.T) {
	out := "nginx.service loaded active running A high performance web server\n● php8.3-fpm.service loaded failed failed The PHP 8.3 FastCGI\ncron.timer loaded active waiting x\n"
	want := []Service{{"nginx", "active", "running"}, {"php8.3-fpm", "failed", "failed"}}
	if got := ParseUnits([]byte(out)); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected: %+v", got)
	}
}

// fake は、決まった出力を返すコマンドの差し替え。実行されたコマンドを記録する。
type fake struct {
	available map[string]bool
	outputs   map[string]string
	globs     map[string][]string
	ran       []string
}

func (f *fake) collector(root string) *Collector {
	return &Collector{
		Root: root,
		Run: func(_ context.Context, _ command.Options, name string, args ...string) ([]byte, error) {
			key := strings.Join(append([]string{name}, args...), " ")
			f.ran = append(f.ran, key)
			if out, ok := f.outputs[key]; ok {
				return []byte(out), nil
			}
			return nil, errors.New("not found")
		},
		LookPath: func(name string) (string, error) {
			if f.available[name] {
				return "/usr/bin/" + name, nil
			}
			return "", command.ErrNotFound
		},
		Glob: func(pattern string) ([]string, error) { return f.globs[pattern], nil },
	}
}

func TestCollect(t *testing.T) {
	f := &fake{
		available: map[string]bool{"dpkg-query": true, "nginx": true, "php": true, "mysqld": true, "redis-server": true, "node": true, "systemctl": true},
		outputs: map[string]string{
			"dpkg-query -W -f=${db:Status-Abbrev}\t${binary:Package}\t${Version}\t${source:Package}\t${source:Version}\t${Architecture}\n": "ii \tzlib1g\t1.3\tzlib\t1.3\tamd64\nii \tbash\t5.2\t\t\tamd64\n",
			"/usr/bin/nginx -v":                             "nginx version: nginx/1.24.0 (Ubuntu)\n",
			"/usr/bin/php -v":                               "PHP 8.3.6 (cli) (built: Apr 15 2024)\nCopyright (c) The PHP Group\n",
			"/usr/bin/mysqld --version":                     "/usr/sbin/mysqld  Ver 10.11.6-MariaDB-0+deb12u1 for debian-linux-gnu on x86_64 (Debian 12)\n",
			"/usr/lib/postgresql/15/bin/postgres --version": "postgres (PostgreSQL) 15.6 (Debian 15.6-0+deb12u1)\n",
			"/usr/lib/postgresql/16/bin/postgres --version": "postgres (PostgreSQL) 16.2\n",
			"/usr/bin/redis-server --version":               "Redis server v=7.0.15 sha=00000000:0 malloc=jemalloc-5.3.0 bits=64\n",
			"/usr/bin/node --version":                       "v20.11.1\n",
			"systemctl list-units --type=service --all --no-legend --no-pager --plain": "nginx.service loaded active running nginx\n",
		},
		globs: map[string][]string{"/usr/lib/postgresql/*/bin/postgres": {"/usr/lib/postgresql/15/bin/postgres", "/usr/lib/postgresql/16/bin/postgres"}},
	}

	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\nVERSION=\"24.04.1 LTS (Noble Numbat)\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\n"), 0o644)
	inv := f.collector(root).Collect(context.Background())

	if want := (OSRelease{"ubuntu", "debian", "24.04", "24.04.1 LTS (Noble Numbat)", "Ubuntu 24.04.1 LTS"}); inv.OSRelease != want {
		t.Errorf("os_release: %+v", inv.OSRelease)
	}
	if want := []Package{{"bash", "5.2", "amd64", "bash", "5.2"}, {"zlib1g", "1.3", "amd64", "zlib", "1.3"}}; !reflect.DeepEqual(inv.Packages, want) {
		t.Errorf("packages: %+v", inv.Packages)
	}
	want := []Middleware{{"nginx", "1.24.0"}, {"php", "8.3.6"}, {"mariadb", "10.11.6"}, {"postgresql", "15.6"}, {"postgresql", "16.2"}, {"redis", "7.0.15"}, {"node", "20.11.1"}}
	if !reflect.DeepEqual(inv.Middleware, want) {
		t.Errorf("middleware: %+v", inv.Middleware)
	}
	if len(inv.Services) != 1 || inv.Services[0].Name != "nginx" || inv.CollectedAt.IsZero() {
		t.Errorf("services: %+v", inv)
	}

	// 実行するのは、決まったコマンドだけ
	allowed := map[string]bool{"dpkg-query": true, "nginx": true, "php": true, "mysqld": true, "postgres": true, "redis-server": true, "node": true, "systemctl": true}
	for _, ran := range f.ran {
		if !allowed[filepath.Base(strings.Fields(ran)[0])] {
			t.Errorf("決まっていないコマンドを実行した: %s", ran)
		}
	}
}

func TestCollectAPKAndLimit(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "lib/apk/db"), 0o755)
	var db strings.Builder
	for i := 0; i < MaxPackages+5; i++ {
		db.WriteString("P:pkg\nV:1\nA:x86_64\n\n")
	}
	os.WriteFile(filepath.Join(root, "lib/apk/db/installed"), []byte(db.String()), 0o644)

	inv := (&fake{}).collector(root).Collect(context.Background())
	if len(inv.Packages) != MaxPackages || !inv.PackagesTruncated {
		t.Fatalf("上限で切らない: %d %v", len(inv.Packages), inv.PackagesTruncated)
	}
	if inv.Middleware == nil || inv.Services == nil {
		t.Fatal("空のときは、nullではなく空の配列にする")
	}
}
