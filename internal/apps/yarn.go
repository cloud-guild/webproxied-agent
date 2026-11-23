package apps

import (
	"strings"
)

// ParseYarnLock は、yarn.lock（v1 と berry）を読む。YAMLの汎用のパーサーは使わず、必要な形だけを読む。
// 直接の依存は、見出しのどれかが package.json に書かれた名前と範囲に一致するもの（名前だけでは、同じ名前の別の版も含んでしまう）。
// yarn.lock には dev の区別が無いため、dev は devDependencies だけに一致した直接の依存だけ true にする。
func ParseYarnLock(data []byte, manifest npmManifest) ([]Package, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	berry := strings.HasPrefix(text, "__metadata:") || strings.Contains(text, "\n__metadata:")
	if !berry && !strings.Contains(text, "# yarn lockfile v1") {
		return nil, errUnsupported
	}

	var list []Package
	var specs []string
	var version, resolution string
	flush := func() {
		defer func() { specs, version, resolution = nil, "", "" }()
		if len(specs) == 0 || version == "" {
			return
		}

		// 見出しの名前（別名のことがある）と、本当の名前
		declared, real, protocol := splitYarnSpec(specs[0])
		if berry && resolution != "" {
			// berry は、resolution（"name@npm:1.2.3"）に本当の名前と取得元がある
			real, _, protocol = splitYarnSpec(resolution)
		}
		switch protocol {
		case "workspace", "link", "portal", "file", "exec":
			// 手元のディレクトリで、公開されたパッケージではない
			return
		case "patch":
			// berry が当てた修正（typescript など）。元のパッケージの項目が別にあるため、重ねて数えない
			return
		}
		if declared == "" || real == "" {
			return
		}

		var prod, dev bool
		for _, s := range specs {
			p, d := manifest.matchSpec(s)
			prod, dev = prod || p, dev || d
		}
		list = append(list, Package{
			Ecosystem: "npm",
			Name:      real,
			Version:   version,
			Direct:    prod || dev,
			Dev:       dev && !prod,
		})
	}

	for _, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line[0] != ' ' {
			// 新しい項目の見出し（"a@^1.0.0", "a@^1.1.0": の形）
			flush()
			header := strings.TrimSuffix(strings.TrimSpace(line), ":")
			if header == "__metadata" {
				continue
			}
			// v1 は "a@^1", "a@^2"、berry は "a@npm:^1, a@npm:^2" と、引用符の付け方が違う
			for _, s := range strings.Split(header, ",") {
				if s = strings.Trim(strings.TrimSpace(s), `"'`); s != "" {
					specs = append(specs, s)
				}
			}
			continue
		}

		// 項目の中の値は、2文字下げた行だけを見る（dependencies の中は、さらに下がっている）
		if strings.HasPrefix(line, "   ") || !strings.HasPrefix(line, "  ") {
			continue
		}
		field := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(field, "version: "):
			version = unquote(strings.TrimSpace(strings.TrimPrefix(field, "version: ")))
		case strings.HasPrefix(field, "version "):
			version = unquote(strings.TrimSpace(strings.TrimPrefix(field, "version ")))
		case strings.HasPrefix(field, "resolution: "):
			resolution = unquote(strings.TrimSpace(strings.TrimPrefix(field, "resolution: ")))
		}
	}
	flush()

	return list, nil
}

// splitYarnSpec は、"name@range"・"name@npm:range"・"alias@npm:real@range" から、書かれた名前・本当の名前・取得元を取り出す。
func splitYarnSpec(spec string) (declared, real, protocol string) {
	name, rest := splitNameAt(spec)
	if name == "" {
		return "", "", ""
	}
	declared, real = name, name

	if p, r, ok := strings.Cut(rest, ":"); ok && !strings.ContainsAny(p, "/^~<>=| ") {
		protocol = p
		// 別名（npm:real@range）なら、本当の名前を取り出す
		if p == "npm" {
			if aliased, _ := splitNameAt(r); aliased != "" && strings.Contains(r[1:], "@") {
				real = aliased
			}
		}
	}

	return declared, real, protocol
}

// splitNameAt は、"@scope/name@rest" や "name@rest" を、名前と残りに分ける。
func splitNameAt(s string) (string, string) {
	start := 0
	if strings.HasPrefix(s, "@") {
		start = 1
	}
	i := strings.Index(s[start:], "@")
	if i < 0 {
		return "", ""
	}
	i += start

	return s[:i], s[i+1:]
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}

	return s
}
