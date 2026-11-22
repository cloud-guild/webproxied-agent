package apps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// makeApp は、Laravel・Vue・WordPressの入ったアプリのディレクトリを作る（releases/1 を current で指す）。
func makeApp(t *testing.T) (base, current string) {
	t.Helper()
	base = t.TempDir()
	release := filepath.Join(base, "releases", "1")

	write(t, filepath.Join(release, "composer.lock"), `{"packages":[{"name":"laravel/framework","version":"v11.31.0"}],"packages-dev":[]}`)
	write(t, filepath.Join(release, "composer.json"), `{"require":{"laravel/framework":"^11.0"}}`)
	write(t, filepath.Join(release, "package.json"), `{"dependencies":{"vue":"^3.5.0"}}`)
	write(t, filepath.Join(release, "package-lock.json"), `{"lockfileVersion":3,"packages":{"":{},"node_modules/vue":{"version":"3.5.12"}}}`)
	// yarn.lock にも同じ版があれば、1つにまとめる
	write(t, filepath.Join(release, "yarn.lock"), "# yarn lockfile v1\n\nvue@^3.5.0:\n  version \"3.5.12\"\n")

	write(t, filepath.Join(release, "wp-includes", "version.php"), "<?php\n$wp_version = '6.8.1';\n")
	// 主ファイルが <スラッグ>.php でないプラグイン
	write(t, filepath.Join(release, "wp-content", "plugins", "contact-form-7", "includes.php"), "<?php\n// no header\n")
	write(t, filepath.Join(release, "wp-content", "plugins", "contact-form-7", "wp-contact-form-7.php"), "<?php\n/*\nPlugin Name: Contact Form 7\nVersion: 6.0.1\n*/\n")
	write(t, filepath.Join(release, "wp-content", "plugins", "index.php"), "<?php // Silence is golden.\n")
	write(t, filepath.Join(release, "wp-content", "themes", "twentytwentyfive", "style.css"), "/*\nTheme Name: Twenty Twenty-Five\nVersion: 1.2\n*/\n")

	// アプリのディレクトリの外を指すリンクは、辿らない
	outside := filepath.Join(base, "outside")
	write(t, filepath.Join(outside, "evil", "evil.php"), "<?php\n/*\nPlugin Name: Evil\nVersion: 9.9\n*/\n")
	if err := os.Symlink(filepath.Join(outside, "evil"), filepath.Join(release, "wp-content", "plugins", "evil")); err != nil {
		t.Fatal(err)
	}
	// 中を指すリンクは辿る
	if err := os.Symlink(filepath.Join(release, "wp-content", "plugins", "contact-form-7"), filepath.Join(release, "wp-content", "plugins", "cf7-link")); err != nil {
		t.Fatal(err)
	}

	current = filepath.Join(base, "current")
	if err := os.Symlink(release, current); err != nil {
		t.Fatal(err)
	}

	return base, current
}

func TestCollectDir(t *testing.T) {
	_, current := makeApp(t)

	app := collectDir(current)
	if app.Error != nil {
		t.Fatalf("unexpected error: %s", *app.Error)
	}
	if app.Path != current {
		t.Fatalf("path should be kept as given: %s", app.Path)
	}
	if want := []string{"composer.lock", "package-lock.json", "yarn.lock", "wordpress"}; !reflect.DeepEqual(app.Sources, want) {
		t.Fatalf("unexpected sources: %v", app.Sources)
	}

	want := []Package{
		{"npm", "vue", "3.5.12", true, false},
		{"packagist", "laravel/framework", "11.31.0", true, false},
		{"wordpress-core", "wordpress", "6.8.1", true, false},
		{"wordpress-plugin", "cf7-link", "6.0.1", true, false},
		{"wordpress-plugin", "contact-form-7", "6.0.1", true, false},
		{"wordpress-theme", "twentytwentyfive", "1.2", true, false},
	}
	if !reflect.DeepEqual(app.Packages, want) {
		t.Fatalf("unexpected packages:\n%+v", app.Packages)
	}

	// JSONの形（error は null、配列は空でも [] ）
	data, _ := json.Marshal(collectDir(t.TempDir()))
	if !strings.Contains(string(data), `"error":null,"sources":[],"truncated":false,"packages":[]`) {
		t.Fatalf("unexpected json: %s", data)
	}
}

func TestCollectDirErrors(t *testing.T) {
	base := t.TempDir()

	if app := collectDir(filepath.Join(base, "missing")); app.Error == nil || *app.Error != "not_found" {
		t.Fatalf("unexpected: %+v", app)
	}
	write(t, filepath.Join(base, "file"), "x")
	if app := collectDir(filepath.Join(base, "file")); app.Error == nil || *app.Error != "not_directory" {
		t.Fatalf("unexpected: %+v", app)
	}

	// 壊れたファイルがあっても、ほかのファイルは読み、最初の理由だけを入れる
	dir := filepath.Join(base, "app")
	write(t, filepath.Join(dir, "composer.lock"), "{broken")
	write(t, filepath.Join(dir, "package-lock.json"), `{"lockfileVersion":1}`)
	write(t, filepath.Join(dir, "pnpm-lock.yaml"), "lockfileVersion: '9.0'\n\npackages:\n\n  vue@3.5.12:\n    resolution: {integrity: x}\n")
	app := collectDir(dir)
	if app.Error == nil || *app.Error != "parse_error:composer.lock" {
		t.Fatalf("unexpected: %+v", app)
	}
	if len(app.Packages) != 1 || app.Packages[0].Name != "vue" || !reflect.DeepEqual(app.Sources, []string{"pnpm-lock.yaml"}) {
		t.Fatalf("unexpected: %+v", app)
	}

	// package-lock.json の lockfileVersion 1 は unsupported
	dir2 := filepath.Join(base, "app2")
	write(t, filepath.Join(dir2, "package-lock.json"), `{"lockfileVersion":1}`)
	if app := collectDir(dir2); app.Error == nil || *app.Error != "unsupported:package-lock.json" {
		t.Fatalf("unexpected: %+v", app)
	}

	// 大きすぎるファイル
	dir3 := filepath.Join(base, "app3")
	write(t, filepath.Join(dir3, "yarn.lock"), "")
	if err := os.Truncate(filepath.Join(dir3, "yarn.lock"), maxFileSize+1); err != nil {
		t.Fatal(err)
	}
	if app := collectDir(dir3); app.Error == nil || *app.Error != "too_large:yarn.lock" {
		t.Fatalf("unexpected: %+v", app)
	}

	// FIFO は開いても待たず、読めないものとする
	dir4 := filepath.Join(base, "app4")
	if err := os.MkdirAll(dir4, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir4, "composer.lock"), 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	if app := collectDir(dir4); app.Error == nil || *app.Error != "read_error:composer.lock" {
		t.Fatalf("unexpected: %+v", app)
	}
}

func TestCollectDirPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root は権限に関係なく読めるため")
	}
	base := t.TempDir()

	locked := filepath.Join(base, "locked")
	write(t, filepath.Join(locked, "composer.lock"), "{}")
	if err := os.Chmod(locked, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o755)
	if app := collectDir(locked); app.Error == nil || *app.Error != "permission_denied" {
		t.Fatalf("unexpected: %+v", app)
	}

	dir := filepath.Join(base, "app")
	write(t, filepath.Join(dir, "composer.lock"), "{}")
	if err := os.Chmod(filepath.Join(dir, "composer.lock"), 0o000); err != nil {
		t.Fatal(err)
	}
	if app := collectDir(dir); app.Error == nil || *app.Error != "permission_denied:composer.lock" {
		t.Fatalf("unexpected: %+v", app)
	}
}

func TestFinish(t *testing.T) {
	pkgs := []Package{
		{"npm", "b", "1.0.0", false, true},
		{"npm", "b", "1.0.0", true, false},
		{"npm", "a\x00\x1b", "2.0.0\n", false, false},
		{"npm", "", "1", false, false},
		{"npm", "c", "1", true, true},
	}
	got, truncated := finish(pkgs, 10)
	want := []Package{{"npm", "a", "2.0.0", false, false}, {"npm", "b", "1.0.0", true, false}, {"npm", "c", "1", true, true}}
	if truncated || !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected: %+v %v", got, truncated)
	}

	// 上限を超えたら、直接の依存を先に残す
	got, truncated = finish(pkgs, 2)
	want = []Package{{"npm", "b", "1.0.0", true, false}, {"npm", "c", "1", true, true}}
	if !truncated || !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected: %+v %v", got, truncated)
	}

	if got := clean(strings.Repeat("あ", 100), maxNameLength); len(got) > maxNameLength || !strings.HasPrefix(got, "あ") || len(got)%3 != 0 {
		t.Fatalf("unexpected: %q", got)
	}
}

func TestLimits(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString(`{"lockfileVersion":3,"packages":{"":{}`)
	for i := 0; i < MaxPackagesPerApp+10; i++ {
		b.WriteString(`,"node_modules/p` + strconv.Itoa(i) + `":{"version":"1.0.0"}`)
	}
	b.WriteString(`}}`)
	write(t, filepath.Join(dir, "package-lock.json"), b.String())

	app := collectDir(dir)
	if !app.Truncated || len(app.Packages) != MaxPackagesPerApp {
		t.Fatalf("unexpected: truncated=%v len=%d", app.Truncated, len(app.Packages))
	}

	// 全体の上限（20,000件）：同じ中身のディレクトリを5つ指定すると、5つ目は0件
	base := t.TempDir()
	var dirs []string
	for i := 0; i < 5; i++ {
		link := filepath.Join(base, "app"+strconv.Itoa(i))
		if err := os.Symlink(dir, link); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, link)
	}
	c := New()
	c.SetDirectories(dirs)
	got := c.Collect(context.Background())
	total := 0
	for _, a := range got {
		total += len(a.Packages)
	}
	if total != MaxPackagesTotal || len(got[4].Packages) != 0 || !got[4].Truncated {
		t.Fatalf("unexpected: total=%d last=%d truncated=%v", total, len(got[4].Packages), got[4].Truncated)
	}
}

func TestValidDirectory(t *testing.T) {
	for p, want := range map[string]bool{
		"/var/www/example/current":            true,
		"/home/kusanagi/example/DocumentRoot": true,
		"relative/path":                       false,
		"/var/www/../etc":                     false,
		"/var/www/a\nb":                       false,
		"":                                    false,
		"/" + strings.Repeat("a", 512):        false,
		"/var/www/..hidden":                   true,
	} {
		if ValidDirectory(p) != want {
			t.Errorf("ValidDirectory(%q) != %v", p, want)
		}
	}
}

func TestCollector(t *testing.T) {
	c := New()
	// 一度も指定されていなければ、送らない
	if got := c.Collect(context.Background()); got != nil {
		t.Fatalf("unexpected: %+v", got)
	}

	_, current := makeApp(t)
	if !c.SetDirectories([]string{current, "relative", current}) {
		t.Fatal("expected changed")
	}
	if c.SetDirectories([]string{current}) {
		t.Fatal("expected unchanged")
	}
	// 順番だけが変わっても、変わっていないものとする
	c.SetDirectories([]string{current, "/srv/other"})
	if c.SetDirectories([]string{"/srv/other", current}) {
		t.Fatal("expected unchanged when only the order differs")
	}
	c.SetDirectories([]string{current})
	got := c.Collect(context.Background())
	if len(got) != 1 || got[0].Error != nil || len(got[0].Packages) != 6 {
		t.Fatalf("unexpected: %+v", got)
	}

	// 空の配列なら、読むのをやめる（空の配列を送る）
	if !c.SetDirectories([]string{}) {
		t.Fatal("expected changed")
	}
	if got := c.Collect(context.Background()); got == nil || len(got) != 0 {
		t.Fatalf("unexpected: %+v", got)
	}

	// 上限は10件
	var many []string
	for i := 0; i < 15; i++ {
		many = append(many, "/srv/app"+strconv.Itoa(i))
	}
	c.SetDirectories(many)
	if len(c.dirs) != MaxDirectories {
		t.Fatalf("unexpected: %d", len(c.dirs))
	}
}

func TestCollectorTimeout(t *testing.T) {
	c := New()
	_, current := makeApp(t)
	other := t.TempDir()
	write(t, filepath.Join(other, "composer.lock"), `{"packages":[{"name":"a/b","version":"1.0.0"}]}`)
	c.SetDirectories([]string{current, other})

	// 前回の読み取りが終わっていないディレクトリ（応答の無いマウントなど）は、待たずに timeout とし、ほかは読む
	hung := make(chan struct{})
	c.running[current] = hung
	start := time.Now()
	got := c.Collect(context.Background())
	if time.Since(start) > 10*time.Second {
		t.Fatal("should not wait for the hung directory")
	}
	if got[0].Error == nil || *got[0].Error != "timeout" || got[1].Error != nil || len(got[1].Packages) != 1 {
		t.Fatalf("unexpected: %+v", got)
	}
	if !TimedOut(got) {
		t.Fatal("expected TimedOut")
	}

	// 終われば、また読む
	close(hung)
	got = c.Collect(context.Background())
	if got[0].Error != nil || TimedOut(got) {
		t.Fatalf("unexpected: %+v", got[0])
	}

	// 時間切れになったら timeout を返し、読み取りが終わるまでは新しく始めない
	c.Timeout = time.Nanosecond
	got = c.Collect(context.Background())
	for _, a := range got {
		if a.Error != nil && *a.Error != "timeout" {
			t.Fatalf("unexpected: %+v", a)
		}
	}
	for _, ch := range c.running {
		<-ch
	}
}
