// Package apps は、サーバー上のアプリケーションのライブラリ・フレームワーク・CMSの版を集める（読み取りだけ）。
// アプリが指定したディレクトリの直下のロックファイル（composer.lock・package-lock.json・yarn.lock・pnpm-lock.yaml）と、
// WordPressの本体・プラグイン・テーマの版を読む。ファイルの中身そのものは送らず、パッケージの名前と版だけを送る。
package apps

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxDirectories は、アプリが指定できるディレクトリの数の上限。
	MaxDirectories = 10
	// maxPathLength は、ディレクトリのパスの長さの上限。
	maxPathLength = 512
	// MaxPackagesPerApp は、1つのディレクトリで送るパッケージの数の上限。
	MaxPackagesPerApp = 5000
	// MaxPackagesTotal は、すべてのディレクトリを合わせて送るパッケージの数の上限。
	MaxPackagesTotal = 20000
	maxNameLength    = 214
	maxVersionLength = 64
)

// errParse は、ファイルが壊れていて読めないとき。
var errParse = errors.New("parse error")

// Application は、1つのアプリケーションのディレクトリから読んだパッケージ。
type Application struct {
	// Path は、アプリが指定した文字列のまま（シンボリックリンクを解決する前）。
	Path string `json:"path"`
	// Error は、読めなかった最初の理由（not_found・permission_denied・parse_error:<ファイル> など）。読めたものは Packages に入れる。
	Error *string `json:"error"`
	// Sources は、読めたもの（composer.lock・package-lock.json・yarn.lock・pnpm-lock.yaml・wordpress）。
	Sources   []string  `json:"sources"`
	Truncated bool      `json:"truncated"`
	Packages  []Package `json:"packages"`
}

// Package は、アプリケーションのパッケージ。
type Package struct {
	// Ecosystem は、packagist・npm・wordpress-core・wordpress-plugin・wordpress-theme のどれか。
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	// Direct は、composer.json・package.json に書かれた直接の依存か（WordPressは、すべて true）。
	Direct bool `json:"direct"`
	// Dev は、開発のときだけの依存か。
	Dev bool `json:"dev"`
}

// ValidDirectory は、アプリが指定したディレクトリのパスとして正しいか（絶対パス・.. を含まない・制御文字なし・512文字まで）。
func ValidDirectory(p string) bool {
	if p == "" || len(p) > maxPathLength || !strings.HasPrefix(p, "/") || !utf8.ValidString(p) {
		return false
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return false
		}
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return false
		}
	}

	return true
}

// Collector は、アプリケーションのパッケージを集める。1つのgoroutineから使う。
type Collector struct {
	// Timeout は、すべてのディレクトリを読むのに待つ時間（応答の無いNFSなどで止まらないよう）。
	Timeout time.Duration

	dirs []string
	set  bool
	// running は、時間切れになったディレクトリの読み取りで、まだ終わっていないもの（終わるまで、そのディレクトリは新しく始めない）。
	running map[string]chan struct{}
}

// New は、Collectorを作る。
func New() *Collector {
	return &Collector{Timeout: time.Minute, running: map[string]chan struct{}{}}
}

// SetDirectories は、読むディレクトリを決める（アプリの応答の app_directories）。正しくないパスは除く。
// 変わったら true を返す（順番だけが変わったときは、変わっていないものとする）。
func (c *Collector) SetDirectories(dirs []string) bool {
	var list []string
	for _, d := range dirs {
		if len(list) >= MaxDirectories {
			break
		}
		if ValidDirectory(d) && !slices.Contains(list, d) {
			list = append(list, d)
		}
	}

	before, after := slices.Clone(c.dirs), slices.Clone(list)
	slices.Sort(before)
	slices.Sort(after)
	changed := !c.set || !slices.Equal(before, after)
	c.dirs, c.set = list, true

	return changed
}

// Collect は、指定されたディレクトリを読む。アプリがまだ一度も指定していなければ nil を返す（送らない）。
// ディレクトリごとに別に読み、時間切れになったものだけを timeout にする（1つの応答の無いマウントで、ほかを止めない）。
func (c *Collector) Collect(ctx context.Context) []Application {
	if !c.set {
		return nil
	}
	if c.running == nil {
		c.running = map[string]chan struct{}{}
	}

	dirs := slices.Clone(c.dirs)
	results := make([]chan Application, len(dirs))
	stillRunning := make([]bool, len(dirs))
	for i, p := range dirs {
		results[i] = make(chan Application, 1)
		if ch, ok := c.running[p]; ok {
			select {
			case <-ch:
				delete(c.running, p)
			default:
				// 前回の読み取りが、まだ終わっていない（待たずに timeout とする）
				stillRunning[i] = true
				results[i] <- failed(p, "timeout")
				continue
			}
		}
		done := make(chan struct{})
		c.running[p] = done
		go func(p string, out chan<- Application) {
			defer close(done)
			out <- collectDir(p)
		}(p, results[i])
	}

	timer := time.NewTimer(c.Timeout)
	defer timer.Stop()
	expired := false

	out := make([]Application, len(dirs))
	remaining := MaxPackagesTotal
	for i, p := range dirs {
		var app Application
		if !expired {
			select {
			case app = <-results[i]:
			case <-timer.C:
				expired = true
			case <-ctx.Done():
				expired = true
			}
		}
		if expired {
			// 待つのをやめた後も、終わっていれば使う
			select {
			case app = <-results[i]:
			default:
				out[i] = failed(p, "timeout")
				continue
			}
		}
		if !stillRunning[i] {
			delete(c.running, p)
		}

		// 全体の上限（直接の依存を先に残す）
		if len(app.Packages) > remaining {
			app.Packages, _ = finish(app.Packages, remaining)
			app.Truncated = true
		}
		remaining -= len(app.Packages)
		out[i] = app
	}

	return out
}

// TimedOut は、時間切れで読めなかったディレクトリがあるか（次の機会に送り直す）。
func TimedOut(list []Application) bool {
	for _, a := range list {
		if a.Error != nil && *a.Error == "timeout" {
			return true
		}
	}

	return false
}

func failed(path, reason string) Application {
	return Application{Path: path, Error: &reason, Sources: []string{}, Packages: []Package{}}
}

// collectDir は、1つのディレクトリを読む（1つのディレクトリの上限まで）。
func collectDir(path string) Application {
	app := Application{Path: path, Sources: []string{}, Packages: []Package{}}

	// 指定されたディレクトリ自体のリンク（current → releases/… など）は辿る
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return failed(path, dirReason(err))
	}
	if info, err := os.Stat(real); err != nil {
		return failed(path, dirReason(err))
	} else if !info.IsDir() {
		return failed(path, "not_directory")
	}
	// 中に入れるか（実行の権限）
	if err := syscall.Access(real, 1); err != nil {
		return failed(path, "permission_denied")
	}
	d, err := openAppDir(real)
	if err != nil {
		return failed(path, dirReason(err))
	}
	defer d.close()

	var first string
	record := func(err error, file string) {
		if reason := fileReason(err, file); reason != "" && first == "" {
			first = reason
		}
	}

	var pkgs []Package
	add := func(source string, list []Package) {
		app.Sources = append(app.Sources, source)
		pkgs = append(pkgs, list...)
	}

	if lock, err := d.readFile("composer.lock"); err == nil {
		manifest, err := d.readFile("composer.json")
		record(err, "composer.json")
		if list, err := ParseComposerLock(lock, manifest); err != nil {
			record(parseError(err), "composer.lock")
		} else {
			add("composer.lock", list)
		}
	} else {
		record(err, "composer.lock")
	}

	manifestData, err := d.readFile("package.json")
	record(err, "package.json")
	manifest := ParsePackageJSON(manifestData)
	for _, lock := range []struct {
		file  string
		parse func([]byte, npmManifest) ([]Package, error)
	}{
		{"package-lock.json", ParsePackageLock},
		{"yarn.lock", ParseYarnLock},
		{"pnpm-lock.yaml", ParsePnpmLock},
	} {
		data, err := d.readFile(lock.file)
		if err != nil {
			record(err, lock.file)
			continue
		}
		if list, err := lock.parse(data, manifest); err != nil {
			record(parseError(err), lock.file)
		} else {
			add(lock.file, list)
		}
	}

	if list, found := d.wordpress(record); found {
		add("wordpress", list)
	}

	app.Packages, app.Truncated = finish(pkgs, MaxPackagesPerApp)
	if first != "" {
		app.Error = &first
	}

	return app
}

// finish は、値を整え（制御文字を除き、長さで切る）、同じものを1つにまとめ、上限までにする（直接の依存を先に残す）。
func finish(pkgs []Package, limit int) ([]Package, bool) {
	merged := map[[3]string]*Package{}
	var list []*Package
	for _, p := range pkgs {
		p.Name = clean(p.Name, maxNameLength)
		p.Version = clean(p.Version, maxVersionLength)
		if p.Name == "" {
			continue
		}
		key := [3]string{p.Ecosystem, p.Name, p.Version}
		if m, ok := merged[key]; ok {
			// どこかで直接の依存なら直接、どこかで本番の依存なら本番
			m.Direct = m.Direct || p.Direct
			m.Dev = m.Dev && p.Dev
			continue
		}
		q := p
		merged[key] = &q
		list = append(list, &q)
	}

	less := func(a, b *Package) bool {
		if a.Ecosystem != b.Ecosystem {
			return a.Ecosystem < b.Ecosystem
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Version < b.Version
	}

	truncated := false
	if len(list) > limit {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Direct != list[j].Direct {
				return list[i].Direct
			}
			return less(list[i], list[j])
		})
		list, truncated = list[:max(limit, 0)], true
	}
	sort.SliceStable(list, func(i, j int) bool { return less(list[i], list[j]) })

	out := make([]Package, len(list))
	for i, p := range list {
		out[i] = *p
	}

	return out, truncated
}

// clean は、制御文字を除き、n バイトまでに切る（文字の途中では切らない）。
func clean(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, strings.TrimSpace(s))

	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}

	return s
}

func dirReason(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not_found"
	case errors.Is(err, fs.ErrPermission):
		return "permission_denied"
	default:
		return "read_error"
	}
}

// fileReason は、ファイルを読めなかった理由。無いファイル・ディレクトリの外を指すリンクは、理由にしない。
func fileReason(err error, file string) string {
	switch {
	case err == nil, errors.Is(err, fs.ErrNotExist), errors.Is(err, errOutside), errors.Is(err, syscall.ENOTDIR):
		return ""
	case errors.Is(err, fs.ErrPermission):
		return "permission_denied:" + file
	case errors.Is(err, errTooLarge):
		return "too_large:" + file
	case errors.Is(err, errUnsupported):
		return "unsupported:" + file
	case errors.Is(err, errParse):
		return "parse_error:" + file
	default:
		return "read_error:" + file
	}
}

func parseError(err error) error {
	if errors.Is(err, errUnsupported) {
		return err
	}

	return errParse
}
