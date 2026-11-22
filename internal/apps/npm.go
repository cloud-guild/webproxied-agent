package apps

import (
	"encoding/json"
	"errors"
	"strings"
)

// errUnsupported は、対応していない形式（package-lock.json の lockfileVersion 1 など）。
var errUnsupported = errors.New("unsupported format")

// npmManifest は、package.json の依存（direct と dev を決めるのに使う）。
type npmManifest struct {
	prod map[string]bool
	dev  map[string]bool
	// prodSpecs・devSpecs は、名前ごとの書かれた範囲（yarn.lock の見出しと照らし合わせる）。
	prodSpecs map[string]string
	devSpecs  map[string]string
}

// ParsePackageJSON は、package.json の dependencies・devDependencies・optionalDependencies・peerDependencies を読む。
// 読めなければ、空のまま返す（direct が分からないだけで、ロックファイルは読む）。
func ParsePackageJSON(data []byte) npmManifest {
	m := npmManifest{prod: map[string]bool{}, dev: map[string]bool{}, prodSpecs: map[string]string{}, devSpecs: map[string]string{}}
	if data == nil {
		return m
	}

	var p struct {
		Dependencies         map[string]json.RawMessage `json:"dependencies"`
		DevDependencies      map[string]json.RawMessage `json:"devDependencies"`
		OptionalDependencies map[string]json.RawMessage `json:"optionalDependencies"`
		PeerDependencies     map[string]json.RawMessage `json:"peerDependencies"`
	}
	if json.Unmarshal(data, &p) != nil {
		return m
	}
	for _, deps := range []map[string]json.RawMessage{p.Dependencies, p.OptionalDependencies, p.PeerDependencies} {
		addDeps(m.prod, m.prodSpecs, deps)
	}
	addDeps(m.dev, m.devSpecs, p.DevDependencies)

	return m
}

// addDeps は、依存の名前を加える。別名（"sw": "npm:string-width@^4.2.0"）は、本当の名前も加える（pnpm は本当の名前で書くため）。
func addDeps(to map[string]bool, specs map[string]string, deps map[string]json.RawMessage) {
	for name, raw := range deps {
		to[name] = true
		var spec string
		if json.Unmarshal(raw, &spec) != nil {
			continue
		}
		specs[name] = spec
		if strings.HasPrefix(spec, "npm:") {
			if real, _ := splitNameAt(strings.TrimPrefix(spec, "npm:")); real != "" {
				to[real] = true
			}
		}
	}
}

// matchSpec は、yarn.lock の見出しの1つ（"name@range"・berry は "name@npm:range"）が、package.json に書かれた依存か。
func (m npmManifest) matchSpec(spec string) (prod, dev bool) {
	name, rest := splitNameAt(spec)
	if name == "" {
		return false, false
	}
	same := func(r string, ok bool) bool {
		return ok && (rest == r || rest == "npm:"+r)
	}
	r, ok := m.prodSpecs[name]
	prod = same(r, ok)
	r, ok = m.devSpecs[name]
	dev = same(r, ok)

	return prod, dev
}

func (m npmManifest) direct(name string) bool {
	return m.prod[name] || m.dev[name]
}

// devOnly は、devDependencies だけにある名前か。
func (m npmManifest) devOnly(name string) bool {
	return m.dev[name] && !m.prod[name]
}

// ParsePackageLock は、package-lock.json（lockfileVersion 2・3 の packages）を読む。
func ParsePackageLock(data []byte, manifest npmManifest) ([]Package, error) {
	var l struct {
		LockfileVersion int `json:"lockfileVersion"`
		Packages        map[string]struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			Dev     bool   `json:"dev"`
			// DevOptional は、開発用で、かつ本番の依存の optional でもあるもの（本番でも入り得る）
			DevOptional bool `json:"devOptional"`
			Link        bool `json:"link"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, err
	}
	// lockfileVersion 1 には packages が無い（npm 6 まで）
	if l.LockfileVersion < 2 || l.Packages == nil {
		return nil, errUnsupported
	}

	var list []Package
	for key, p := range l.Packages {
		// "" は、アプリ自身。node_modules の無いキーは、ワークスペースのディレクトリ
		i := strings.LastIndex(key, "node_modules/")
		if key == "" || i < 0 || p.Link || p.Version == "" {
			continue
		}
		installed := key[i+len("node_modules/"):]
		name := installed
		// 別名で入れたもの（"alias": "npm:real@1.0.0"）は、本当の名前を送る
		if p.Name != "" {
			name = p.Name
		}
		topLevel := key == "node_modules/"+installed
		direct := topLevel && manifest.direct(installed)
		list = append(list, Package{
			Ecosystem: "npm",
			Name:      name,
			Version:   p.Version,
			Direct:    direct,
			// devDependencies だけに書かれた直接の依存は、devOptional でも開発用とする
			Dev: p.Dev || direct && p.DevOptional && manifest.devOnly(installed),
		})
	}

	return list, nil
}
