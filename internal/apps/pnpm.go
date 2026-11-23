package apps

import (
	"strconv"
	"strings"
)

// ParsePnpmLock は、pnpm-lock.yaml（lockfileVersion 6〜9）の packages を読む。YAMLの汎用のパーサーは使わず、必要な形だけを読む。
//
//	v6: "  /@babel/parser@7.22.10:" の下に "    dev: false"
//	v9: "  '@babel/parser@7.26.2':"（dev の区別は無い）
//
// 直接の依存は、importers（v6 の1つのプロジェクトでは、先頭の dependencies・devDependencies）に書かれた名前と版に一致するもの。
func ParsePnpmLock(data []byte, manifest npmManifest) ([]Package, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")

	supported := false
	for _, line := range lines {
		if v, ok := strings.CutPrefix(line, "lockfileVersion:"); ok {
			major, _, _ := strings.Cut(unquote(strings.TrimSpace(v)), ".")
			n, err := strconv.Atoi(major)
			supported = err == nil && n >= 6 && n <= 9
			break
		}
	}
	if !supported {
		return nil, errUnsupported
	}

	prodDirect, devDirect := pnpmDirect(lines)

	var list []Package
	inPackages := false
	var key, name, version string
	var dev, devKnown bool
	flush := func() {
		defer func() { key, name, version, dev, devKnown = "", "", "", false, false }()
		if key == "" {
			return
		}

		// キーは "/name@1.0.0(peer@2.0.0)" や "name@1.0.0"。ほかの取得元（git・tarball）は、中の name・version を使う
		keyName, keyVersion := splitPnpmRef(unquote(key))
		if name == "" {
			name = keyName
		}
		if version == "" {
			version = keyVersion
		}
		if name == "" || version == "" || strings.Contains(version, ":") || strings.Contains(version, "/") {
			return
		}

		// importers の版は、キーの版（git などは、キーそのもの）で照らし合わせる
		ref := keyName + "@" + keyVersion
		prod, devOnly := prodDirect[ref], devDirect[ref]
		if len(prodDirect) == 0 && len(devDirect) == 0 {
			// importers が無い（読めない）ときは、名前で判断する
			prod, devOnly = manifest.prod[name], manifest.dev[name]
		}
		if !devKnown {
			dev = devOnly && !prod
		}
		list = append(list, Package{Ecosystem: "npm", Name: name, Version: version, Direct: prod || devOnly, Dev: dev})
	}

	for _, line := range lines {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}

		// 先頭の見出し（packages: ・snapshots: など）
		if line[0] != ' ' {
			flush()
			inPackages = strings.TrimSpace(line) == "packages:"
			continue
		}
		if !inPackages {
			continue
		}

		switch {
		case strings.HasPrefix(line, "  ") && line[2] != ' ':
			// パッケージの見出し
			flush()
			key = strings.TrimSuffix(strings.TrimSpace(line), ":")
		case strings.HasPrefix(line, "    ") && line[4] != ' ':
			field, value, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok {
				continue
			}
			value = unquote(strings.TrimSpace(value))
			switch field {
			case "name":
				name = value
			case "version":
				version = value
			case "dev":
				dev, devKnown = value == "true", true
			}
		}
	}
	flush()

	return list, nil
}

// splitPnpmRef は、"/name@1.0.0(peer@2.0.0)" を、名前と版に分ける。
func splitPnpmRef(ref string) (string, string) {
	ref = strings.TrimPrefix(ref, "/")
	if i := strings.Index(ref, "("); i > 0 {
		ref = ref[:i]
	}

	return splitNameAt(ref)
}

// pnpmDirect は、直接の依存（"name@version"）を読む。importers の下（v9・v6 のワークスペース）と、先頭（v6 の1つのプロジェクト）の
// dependencies・optionalDependencies（本番）と devDependencies（開発用）にある
//
//	vue:
//	  specifier: ^3.5.12
//	  version: 3.5.12(typescript@5.6.3)
//
// の形。別名は "version: string-width@4.2.3"、v6 では "/string-width@4.2.3" になる。
func pnpmDirect(lines []string) (prod, dev map[string]bool) {
	prod, dev = map[string]bool{}, map[string]bool{}

	inImporters := false
	sectionIndent := -1
	var target map[string]bool
	var depName string
	depIndent := -1
	add := func(version string) {
		version = unquote(strings.TrimSpace(version))
		// ピアの依存の印（"3.5.12(typescript@5.6.3)"）を除く
		if i := strings.Index(version, "("); i > 0 {
			version = version[:i]
		}
		if depName == "" || version == "" || target == nil || strings.HasPrefix(version, "link:") {
			return
		}
		ref := depName + "@" + version
		// 別名（string-width@4.2.3・/string-width@4.2.3）は、版に本当の名前が入っている
		if v := strings.TrimPrefix(version, "/"); len(v) > 1 && strings.Contains(v[1:], "@") {
			ref = v
		}
		if name, ver := splitPnpmRef(ref); name != "" {
			target[name+"@"+ver] = true
		}
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		if indent == 0 {
			inImporters = trimmed == "importers:"
			target, sectionIndent, depName = nil, -1, ""
			// v6 の1つのプロジェクトは、先頭に dependencies: などがある
			switch trimmed {
			case "dependencies:", "optionalDependencies:":
				target, sectionIndent = prod, 0
			case "devDependencies:":
				target, sectionIndent = dev, 0
			}
			continue
		}

		if inImporters {
			switch trimmed {
			case "dependencies:", "optionalDependencies:":
				target, sectionIndent, depName = prod, indent, ""
				continue
			case "devDependencies:":
				target, sectionIndent, depName = dev, indent, ""
				continue
			}
		}
		if target == nil || indent <= sectionIndent {
			if indent <= sectionIndent {
				target, sectionIndent = nil, -1
			}
			continue
		}

		field, value, _ := strings.Cut(trimmed, ":")
		switch {
		case indent == sectionIndent+2:
			// 依存の名前（古い形の "name: 1.0.0" は、そのまま版）
			depName, depIndent = unquote(strings.TrimSpace(field)), indent
			if v := strings.TrimSpace(value); v != "" {
				add(v)
			}
		case indent == depIndent+2 && field == "version":
			add(value)
		}
	}

	return prod, dev
}
