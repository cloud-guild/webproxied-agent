package apps

import (
	"path"
	"regexp"
	"strings"
)

const (
	// maxPlugins は、1つのアプリで読むプラグイン・テーマの数の上限。
	maxPlugins = 2000
	// maxPluginFiles は、プラグインの主ファイルを探すとき、1つのプラグインで読む .php の数の上限。
	maxPluginFiles = 50
	// headerSize は、プラグイン・テーマの見出し（Plugin Name: など）を探す、ファイルの先頭の大きさ（WordPressと同じ8KB）。
	headerSize = 8 << 10
)

var wpVersion = regexp.MustCompile(`\$wp_version\s*=\s*['"]([^'"]+)['"]`)

// ParseWPVersion は、wp-includes/version.php の $wp_version を読む。
func ParseWPVersion(data []byte) string {
	if m := wpVersion.FindSubmatch(data); m != nil {
		return string(m[1])
	}

	return ""
}

// wpHeaders は、読む見出し（WordPressの get_file_data と同じ形）。
var wpHeaders = map[string]*regexp.Regexp{
	"Plugin Name": wpHeader("Plugin Name"),
	"Version":     wpHeader("Version"),
}

func wpHeader(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?mi)^(?:[ \t]*<\?php)?[ \t/*#@]*` + regexp.QuoteMeta(name) + `:(.*)$`)
}

// ParseWPHeader は、プラグイン・テーマのファイルの見出し（"Version: 1.0" など）を、WordPressの get_file_data と同じ決まりで読む。
func ParseWPHeader(data []byte, name string) (string, bool) {
	re, ok := wpHeaders[name]
	if !ok {
		re = wpHeader(name)
	}
	m := re.FindSubmatch(data)
	if m == nil {
		return "", false
	}

	// コメントの終わり（*/）や ?> より後ろは除く
	v := string(m[1])
	if i := strings.Index(v, "*/"); i >= 0 {
		v = v[:i]
	}
	if i := strings.Index(v, "?>"); i >= 0 {
		v = v[:i]
	}

	return strings.TrimSpace(v), true
}

// wordpress は、WordPressの本体・プラグイン・テーマの版を読む。読めなかったファイルは record に渡す。
func (d appDir) wordpress(record func(error, string)) ([]Package, bool) {
	var list []Package
	found := false

	if data, err := d.readHead("wp-includes/version.php", 64<<10); err == nil {
		if v := ParseWPVersion(data); v != "" {
			found = true
			list = append(list, Package{Ecosystem: "wordpress-core", Name: "wordpress", Version: v, Direct: true})
		} else {
			record(errParse, "wp-includes/version.php")
		}
	} else {
		record(err, "wp-includes/version.php")
	}

	if !found && !d.exists("wp-content") {
		return nil, false
	}

	plugins, err := d.subdirs("wp-content/plugins", maxPlugins)
	if err != nil {
		record(err, "wp-content/plugins")
	}
	for _, slug := range plugins {
		if v, ok := d.pluginVersion(slug, record); ok {
			found = true
			list = append(list, Package{Ecosystem: "wordpress-plugin", Name: slug, Version: v, Direct: true})
		}
	}

	themes, err := d.subdirs("wp-content/themes", maxPlugins)
	if err != nil {
		record(err, "wp-content/themes")
	}
	for _, slug := range themes {
		file := path.Join("wp-content/themes", slug, "style.css")
		data, err := d.readHead(file, headerSize)
		if err != nil {
			record(err, file)
			continue
		}
		v, _ := ParseWPHeader(data, "Version")
		found = true
		list = append(list, Package{Ecosystem: "wordpress-theme", Name: slug, Version: v, Direct: true})
	}

	return list, found
}

// pluginVersion は、プラグインの主ファイル（直下の .php のうち Plugin Name: を持つもの。<スラッグ>.php を先に見る）の Version: を読む。
func (d appDir) pluginVersion(slug string, record func(error, string)) (string, bool) {
	dir := path.Join("wp-content/plugins", slug)
	files, err := d.files(dir, ".php", maxPluginFiles)
	if err != nil {
		record(err, dir)
		return "", false
	}

	ordered := make([]string, 0, len(files))
	for _, f := range files {
		if f == slug+".php" {
			ordered = append([]string{f}, ordered...)
		} else {
			ordered = append(ordered, f)
		}
	}

	for _, f := range ordered {
		file := path.Join(dir, f)
		data, err := d.readHead(file, headerSize)
		if err != nil {
			record(err, file)
			continue
		}
		if _, ok := ParseWPHeader(data, "Plugin Name"); ok {
			v, _ := ParseWPHeader(data, "Version")
			return v, true
		}
	}

	return "", false
}
