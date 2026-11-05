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
	out := "nginx\t1.24.0-2ubuntu7\tamd64\tinstall ok installed\nold\t1.0\tall\tdeinstall ok config-files\nlibc6\t2.39\tamd64\tinstall ok installed\n"
	want := []Package{{"nginx", "1.24.0-2ubuntu7", "amd64"}, {"libc6", "2.39", "amd64"}}
	if got := ParseDpkg([]byte(out)); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestParseRPM(t *testing.T) {
	out := "openssl\t1:3.0.7-27.el9\tx86_64\ngpg-pubkey\t3228467c-613798eb\t(none)\nbash\t5.1.8-9.el9\tx86_64\n"
	want := []Package{{"openssl", "1:3.0.7-27.el9", "x86_64"}, {"bash", "5.1.8-9.el9", "x86_64"}}
	if got := ParseRPM([]byte(out)); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestParseAPK(t *testing.T) {
	db := "C:Q1abc\nP:musl\nV:1.2.5-r0\nA:x86_64\nS:1\n\nP:busybox\nV:1.36.1-r29\nA:x86_64\n"
	want := []Package{{"musl", "1.2.5-r0", "x86_64"}, {"busybox", "1.36.1-r29", "x86_64"}}
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
			"dpkg-query -W -f=${Package}\t${Version}\t${Architecture}\t${Status}\n": "zlib1g\t1.3\tamd64\tinstall ok installed\nbash\t5.2\tamd64\tinstall ok installed\n",
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

	inv := f.collector(t.TempDir()).Collect(context.Background())

	if want := []Package{{"bash", "5.2", "amd64"}, {"zlib1g", "1.3", "amd64"}}; !reflect.DeepEqual(inv.Packages, want) {
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
