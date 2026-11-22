package apps

import (
	"encoding/json"
	"strings"
)

// ParseComposerLock は、composer.lock を読む（packages は本番、packages-dev は dev）。
// manifest（composer.json）の require・require-dev にある名前は、direct にする。中身の認証情報（auth など）は読まない。
func ParseComposerLock(lock, manifest []byte) ([]Package, error) {
	var l struct {
		Packages    []composerPackage `json:"packages"`
		PackagesDev []composerPackage `json:"packages-dev"`
	}
	if err := json.Unmarshal(lock, &l); err != nil {
		return nil, err
	}

	direct := map[string]bool{}
	if manifest != nil {
		var m struct {
			Require    map[string]json.RawMessage `json:"require"`
			RequireDev map[string]json.RawMessage `json:"require-dev"`
		}
		// composer.json が壊れていても、composer.lock は読む（direct が分からないだけ）
		if json.Unmarshal(manifest, &m) == nil {
			for name := range m.Require {
				direct[strings.ToLower(name)] = true
			}
			for name := range m.RequireDev {
				direct[strings.ToLower(name)] = true
			}
		}
	}

	var list []Package
	add := func(pkgs []composerPackage, dev bool) {
		for _, p := range pkgs {
			if p.Name == "" || p.Version == "" {
				continue
			}
			list = append(list, Package{
				Ecosystem: "packagist",
				Name:      p.Name,
				Version:   strings.TrimPrefix(p.Version, "v"),
				Direct:    direct[strings.ToLower(p.Name)],
				Dev:       dev,
			})
		}
	}
	add(l.Packages, false)
	add(l.PackagesDev, true)

	return list, nil
}

type composerPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
