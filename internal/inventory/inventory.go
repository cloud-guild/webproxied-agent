// Package inventory は、分析のためのサーバーの情報（入っているパッケージ・ミドルウェアの版・サービスの一覧）を集める。
// 読み取りだけを行い、実行するのは、このファイルに書いた決まったコマンドだけ。
package inventory

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cloud-guild/webproxied-agent/internal/command"
	"github.com/cloud-guild/webproxied-agent/internal/host"
)

// MaxPackages は、送るパッケージの数の上限。
const MaxPackages = 20000

// Inventory は、サーバーの情報。
type Inventory struct {
	CollectedAt time.Time `json:"collected_at"`
	// OSRelease は、/etc/os-release の値（OSV.devのエコシステム名を決めるのに使う）。
	OSRelease OSRelease `json:"os_release"`
	Packages  []Package `json:"packages"`
	// PackagesTruncated は、上限（MaxPackages）を超えたため、一部だけを送ったとき。
	PackagesTruncated bool         `json:"packages_truncated,omitempty"`
	Middleware        []Middleware `json:"middleware"`
	Services          []Service    `json:"services"`
}

// OSRelease は、/etc/os-release の値（読めない項目は空の文字列）。
type OSRelease struct {
	ID         string `json:"ID"`
	IDLike     string `json:"ID_LIKE"`
	VersionID  string `json:"VERSION_ID"`
	Version    string `json:"VERSION"`
	PrettyName string `json:"PRETTY_NAME"`
}

// Package は、入っているパッケージ。脆弱性の照合（OSV.dev）のため、ソースのパッケージの名前と版も持つ。
type Package struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	Arch          string `json:"arch"`
	SourceName    string `json:"source_name"`
	SourceVersion string `json:"source_version"`
}

// Middleware は、ミドルウェアの版。
type Middleware struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Service は、systemdのサービス（名前は、末尾の .service を除いたもの）。
type Service struct {
	Name        string `json:"name"`
	ActiveState string `json:"active_state"`
	SubState    string `json:"sub_state"`
}

// Collector は、サーバーの情報を集める。
type Collector struct {
	// Root は、ファイルを読む場所（テストで差し替える）。
	Root string
	// Run は、コマンドの実行（テストで差し替える）。
	Run command.Runner
	// LookPath は、実行ファイルを探す（テストで差し替える）。
	LookPath func(name string) (string, error)
	// Glob は、決まった場所にある実行ファイルを探す（テストで差し替える）。
	Glob func(pattern string) ([]string, error)
}

// New は、実際のサーバーを読むCollectorを作る。
func New() *Collector {
	return &Collector{Root: "/", Run: command.Run, LookPath: command.LookPath, Glob: filepath.Glob}
}

// Collect は、サーバーの情報を集める。読めない項目は空のまま返す。
func (c *Collector) Collect(ctx context.Context) Inventory {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	inv := Inventory{CollectedAt: time.Now().UTC(), OSRelease: c.osRelease()}
	inv.Packages, inv.PackagesTruncated = c.packages(ctx)
	inv.Middleware = c.middleware(ctx)
	inv.Services = c.services(ctx)

	return inv
}

func (c *Collector) osRelease() OSRelease {
	f, err := os.Open(filepath.Join(c.Root, "etc/os-release"))
	if err != nil {
		return OSRelease{}
	}
	defer f.Close()

	v := host.ParseOSRelease(f)
	return OSRelease{ID: v["ID"], IDLike: v["ID_LIKE"], VersionID: v["VERSION_ID"], Version: v["VERSION"], PrettyName: v["PRETTY_NAME"]}
}

// パッケージの一覧の出力の上限（2万件 × 1件あたりの長さ）
var packageOptions = command.Options{Timeout: 60 * time.Second, Limit: 16 << 20}

func (c *Collector) packages(ctx context.Context) ([]Package, bool) {
	var list []Package

	switch {
	case c.has("dpkg-query"):
		out, _ := c.Run(ctx, packageOptions, "dpkg-query", "-W", "-f=${db:Status-Abbrev}\t${binary:Package}\t${Version}\t${source:Package}\t${source:Version}\t${Architecture}\n")
		list = ParseDpkg(out)
	case c.has("rpm"):
		out, _ := c.Run(ctx, packageOptions, "rpm", "-qa", "--qf", "%{NAME}\t%{EPOCHNUM}:%{VERSION}-%{RELEASE}\t%{ARCH}\t%{SOURCERPM}\n")
		list = ParseRPM(out)
	default:
		// Alpine Linux は、apkのデータベースのファイルを読む（コマンドの出力は、名前と版の区切りが曖昧なため）
		if f, err := os.Open(filepath.Join(c.Root, "lib/apk/db/installed")); err == nil {
			list = ParseAPK(f)
			f.Close()
		}
	}

	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	if len(list) > MaxPackages {
		return list[:MaxPackages], true
	}

	return list, false
}

func (c *Collector) has(name string) bool {
	_, err := c.LookPath(name)
	return err == nil
}

// ParseDpkg は、dpkg-query の出力（状態・名前・版・ソースの名前・ソースの版・アーキテクチャのタブ区切り）を読む。
// 入っているもの（状態が ii）だけを返す。ソースが空なら、パッケージの名前と版を使う。
func ParseDpkg(out []byte) []Package {
	var list []Package
	for _, line := range lines(out) {
		fields := strings.Split(line, "\t")
		if len(fields) != 6 || !strings.HasPrefix(fields[0], "ii") || fields[1] == "" {
			continue
		}

		// binary:Package は、複数のアーキテクチャを入れられるパッケージに「:amd64」などが付く（アーキテクチャは別に送る）
		name, _, _ := strings.Cut(fields[1], ":")
		p := Package{Name: name, Version: fields[2], Arch: fields[5], SourceName: fields[3], SourceVersion: fields[4]}
		if p.SourceName == "" {
			p.SourceName = name
		}
		if p.SourceVersion == "" {
			p.SourceVersion = p.Version
		}
		list = append(list, p)
	}

	return list
}

// sourceRPM は、SOURCERPM（例: openssl-3.0.7-27.el9.src.rpm）から、ソースのパッケージの名前を取り出す。
var sourceRPM = regexp.MustCompile(`^(.+)-[^-]+-[^-]+\.src\.rpm$`)

// ParseRPM は、rpm -qa の出力（名前・エポック:版-リリース・アーキテクチャ・SOURCERPMのタブ区切り）を読む。
// エポックが0なら付けない（OSV.devの版の表記に合わせる）。ソースの版は、パッケージの版と同じ。
func ParseRPM(out []byte) []Package {
	var list []Package
	for _, line := range lines(out) {
		fields := strings.Split(line, "\t")
		// gpg-pubkey は、署名の鍵（パッケージではない）
		if len(fields) != 4 || fields[0] == "" || fields[0] == "gpg-pubkey" {
			continue
		}

		version := strings.TrimPrefix(fields[1], "0:")
		source := fields[0]
		if m := sourceRPM.FindStringSubmatch(fields[3]); m != nil {
			source = m[1]
		}
		list = append(list, Package{Name: fields[0], Version: version, Arch: fields[2], SourceName: source, SourceVersion: version})
	}

	return list
}

// ParseAPK は、/lib/apk/db/installed（P:名前・V:版・A:アーキテクチャ・o:元のパッケージの行、空行で区切る）を読む。
// 元のパッケージ（origin。apk list --installed の {…} と同じ）を、ソースのパッケージの名前にする。
func ParseAPK(r interface{ Read([]byte) (int, error) }) []Package {
	var list []Package
	var current Package

	flush := func() {
		if current.Name != "" {
			if current.SourceName == "" {
				current.SourceName = current.Name
			}
			current.SourceVersion = current.Version
			list = append(list, current)
		}
		current = Package{}
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "P:"):
			current.Name = line[2:]
		case strings.HasPrefix(line, "V:"):
			current.Version = line[2:]
		case strings.HasPrefix(line, "A:"):
			current.Arch = line[2:]
		case strings.HasPrefix(line, "o:"):
			current.SourceName = line[2:]
		}
	}
	flush()

	return list
}

// probe は、ミドルウェアの版を調べるコマンド（決まったもの）。
type probe struct {
	name string
	// commands は、試すコマンド（名前は SearchDirs から探す。絶対パスのglobは、決まった場所だけ）
	commands []string
	globs    []string
	args     []string
	pattern  *regexp.Regexp
	// rename は、出力から名前を決める（mysqlとmariadbの区別）
	rename func(out []byte) string
}

var probes = []probe{
	{name: "nginx", commands: []string{"nginx"}, args: []string{"-v"}, pattern: regexp.MustCompile(`nginx/(\d+(?:\.\d+)+)`)},
	{name: "apache", commands: []string{"apache2", "httpd"}, args: []string{"-v"}, pattern: regexp.MustCompile(`Apache/(\d+(?:\.\d+)+)`)},
	{name: "php", commands: []string{"php", "php-fpm"}, globs: []string{"/usr/sbin/php-fpm[0-9]*"}, args: []string{"-v"}, pattern: regexp.MustCompile(`PHP (\d+(?:\.\d+)+)`)},
	{
		name: "mysql", commands: []string{"mariadbd", "mysqld"}, globs: []string{"/usr/libexec/mariadbd", "/usr/libexec/mysqld"}, args: []string{"--version"},
		pattern: regexp.MustCompile(`Ver (\d+(?:\.\d+)+)`),
		rename: func(out []byte) string {
			if bytes.Contains(out, []byte("MariaDB")) {
				return "mariadb"
			}
			return "mysql"
		},
	},
	// Debian・UbuntuやPGDGのパッケージは、PATHの外に版ごとに入る（複数の版があれば、それぞれを送る）
	{name: "postgresql", commands: []string{"postgres"}, globs: []string{"/usr/lib/postgresql/*/bin/postgres", "/usr/pgsql-*/bin/postgres"}, args: []string{"--version"}, pattern: regexp.MustCompile(`PostgreSQL\)? (\d+(?:\.\d+)*)`)},
	{name: "redis", commands: []string{"redis-server"}, args: []string{"--version"}, pattern: regexp.MustCompile(`v=(\d+(?:\.\d+)+)`)},
	{name: "node", commands: []string{"node", "nodejs"}, args: []string{"--version"}, pattern: regexp.MustCompile(`^v(\d+(?:\.\d+)+)`)},
}

var versionOptions = command.Options{Timeout: 10 * time.Second, Limit: 64 << 10, Stderr: true}

func (c *Collector) middleware(ctx context.Context) []Middleware {
	list := []Middleware{}

	for _, p := range probes {
		var paths []string
		for _, name := range p.commands {
			if path, err := c.LookPath(name); err == nil {
				paths = append(paths, path)
				break
			}
		}
		if len(paths) == 0 || p.name == "postgresql" {
			for _, pattern := range p.globs {
				matches, _ := c.Glob(pattern)
				paths = append(paths, matches...)
			}
		}

		seen := map[string]bool{}
		for _, path := range dedupe(paths) {
			out, _ := c.Run(ctx, versionOptions, path, p.args...)
			m := p.pattern.FindSubmatch(bytes.TrimSpace(out))
			if m == nil {
				continue
			}

			name := p.name
			if p.rename != nil {
				name = p.rename(out)
			}
			key := name + " " + string(m[1])
			if !seen[key] {
				seen[key] = true
				list = append(list, Middleware{Name: name, Version: string(m[1])})
			}
			if p.name != "postgresql" {
				break
			}
		}
	}

	return list
}

func dedupe(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}

	return out
}

func (c *Collector) services(ctx context.Context) []Service {
	if !c.has("systemctl") {
		return []Service{}
	}

	out, _ := c.Run(ctx, command.Options{Timeout: 20 * time.Second, Limit: 4 << 20}, "systemctl", "list-units", "--type=service", "--all", "--no-legend", "--no-pager", "--plain")
	return ParseUnits(out)
}

// ParseUnits は、systemctl list-units の出力（ユニット・LOAD・ACTIVE・SUB・説明）を読む。
func ParseUnits(out []byte) []Service {
	list := []Service{}
	for _, line := range lines(out) {
		fields := strings.Fields(line)
		// 失敗したユニットなどの先頭に付く印
		if len(fields) > 0 && (fields[0] == "●" || fields[0] == "*") {
			fields = fields[1:]
		}
		if len(fields) < 4 || !strings.HasSuffix(fields[0], ".service") {
			continue
		}
		list = append(list, Service{Name: strings.TrimSuffix(fields[0], ".service"), ActiveState: fields[2], SubState: fields[3]})
	}

	return list
}

func lines(out []byte) []string {
	return strings.FieldsFunc(string(out), func(r rune) bool { return r == '\n' || r == '\r' })
}
