package sshd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// want は、期待する値（"値@出どころ"、または "!理由"）。
type want struct{ root, password, kbd, pam string }

func check(t *testing.T, got *Status, w want) {
	t.Helper()
	for _, c := range []struct {
		name string
		s    *Setting
		want string
	}{
		{"permit_root_login", got.PermitRootLogin, w.root},
		{"password_authentication", got.PasswordAuthentication, w.password},
		{"kbd_interactive_authentication", got.KbdInteractiveAuthentication, w.kbd},
		{"use_pam", got.UsePAM, w.pam},
	} {
		actual := c.s.Value + "@" + c.s.Source
		if c.s.Error != "" {
			actual = "!" + c.s.Error
		}
		if actual != c.want {
			t.Errorf("%s: %s（期待 %s）", c.name, actual, c.want)
		}
	}
}

// writeFiles は、root の下に設定ファイルを作る。
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// 本物の設定ファイル（各OSの openssh-server を入れた直後）。期待する値は、そのOSの sshd -T の出力
// （sshd -T は、既定の prohibit-password を without-password と表示する）
func TestRealConfigs(t *testing.T) {
	cases := []struct {
		dir  string
		want want
	}{
		{"ubuntu-24.04", want{"prohibit-password@default", "yes@default", "no@/etc/ssh/sshd_config:71", "yes@/etc/ssh/sshd_config:94"}},
		// 50-redhat.conf の ChallengeResponseAuthentication は、KbdInteractiveAuthentication の古い名前
		{"rocky-9", want{"prohibit-password@default", "yes@default", "no@/etc/ssh/sshd_config.d/50-redhat.conf:10", "yes@/etc/ssh/sshd_config.d/50-redhat.conf:15"}},
		{"almalinux-8", want{"yes@/etc/ssh/sshd_config:44", "yes@/etc/ssh/sshd_config:71", "no@/etc/ssh/sshd_config:75", "yes@/etc/ssh/sshd_config:102"}},
	}
	for _, c := range cases {
		t.Run(c.dir, func(t *testing.T) {
			check(t, Collect(filepath.Join("testdata", c.dir), true), c.want)
		})
	}
}

// sshd -T で確かめた組み合わせ（Ubuntu 24.04・Debian 12・Rocky 9・Amazon Linux 2023・AlmaLinux 8 で同じ結果）
func TestOrderAndMatch(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"etc/ssh/sshd_config": "Include /etc/ssh/sshd_config.d/*.conf\n" +
			"# Match は、Include したファイルの終わりで終わる\n" +
			"UsePAM yes\n" +
			"PermitRootLogin no\n",
		"etc/ssh/sshd_config.d/10-a.conf": "PasswordAuthentication no\nPermitRootLogin=yes   # 注釈\n",
		"etc/ssh/sshd_config.d/20-b.conf": "PasswordAuthentication yes\nchallengeresponseauthentication yes\n" +
			"Match User foo\n  PasswordAuthentication yes\n  PermitRootLogin no\n",
		"etc/ssh/sshd_config.d/30-c.conf": "KbdInteractiveAuthentication no\n",
		// . で始まるファイルは、glob に一致しない
		"etc/ssh/sshd_config.d/.hidden.conf":  "UsePAM no\n",
		"etc/ssh/sshd_config.d/40-d.conf.bak": "UsePAM no\n",
	})

	got := Collect(root, true)
	check(t, got, want{
		"yes@/etc/ssh/sshd_config.d/10-a.conf:2",
		"no@/etc/ssh/sshd_config.d/10-a.conf:1",
		"yes@/etc/ssh/sshd_config.d/20-b.conf:2",
		"yes@/etc/ssh/sshd_config:3",
	})
	if !reflect.DeepEqual(got.PasswordAuthentication.MatchValues, []string{"yes"}) || !reflect.DeepEqual(got.PermitRootLogin.MatchValues, []string{"no"}) ||
		!reflect.DeepEqual(got.UsePAM.MatchValues, []string{}) || !reflect.DeepEqual(got.KbdInteractiveAuthentication.MatchValues, []string{}) {
		t.Errorf("match_values: %+v %+v %+v", got.PasswordAuthentication, got.PermitRootLogin, got.UsePAM)
	}
}

// "Match all" は、すべての接続に一致するため、その値が Match の外の値に代わる（sshd -T で確かめた）
func TestMatchAll(t *testing.T) {
	cases := []struct {
		name, config string
		want         want
		match        []string
	}{
		{
			name: "条件付きと all",
			config: "PasswordAuthentication no\nMatch Address 10.0.0.0/8\n PasswordAuthentication yes\n PermitRootLogin yes\n" +
				"Match all\n PermitRootLogin \"forced-commands-only\"\n",
			want:  want{"forced-commands-only@/etc/ssh/sshd_config:6", "no@/etc/ssh/sshd_config:1", "yes@default", "no@default"},
			match: []string{"yes"},
		},
		{
			name:   "all が外の値に代わる",
			config: "PermitRootLogin no\nMatch all\n PermitRootLogin yes\n",
			want:   want{"yes@/etc/ssh/sshd_config:3", "yes@default", "yes@default", "no@default"},
			match:  []string{},
		},
		{
			// all の中でも最初の値が勝ち、all で決まった後の条件付きの Match は効かない
			name:   "all の後の条件付き",
			config: "Match all\n PermitRootLogin yes\n PermitRootLogin no\nMatch User x\n PermitRootLogin forced-commands-only\n",
			want:   want{"yes@/etc/ssh/sshd_config:2", "yes@default", "yes@default", "no@default"},
			match:  []string{},
		},
		{
			// UsePAM は Match の中で使えない（sshd は起動しない）ため、Match の中の値は見ない
			name:   "Match の中の UsePAM",
			config: "UsePAM yes\nMatch all\n UsePAM no\n",
			want:   want{"prohibit-password@default", "yes@default", "yes@default", "yes@/etc/ssh/sshd_config:1"},
			match:  []string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Collect(writeFiles(t, map[string]string{"etc/ssh/sshd_config": c.config}), true)
			check(t, got, c.want)
			if !reflect.DeepEqual(got.PermitRootLogin.MatchValues, c.match) {
				t.Errorf("match_values: %+v", got.PermitRootLogin.MatchValues)
			}
		})
	}

	// 条件付きの Match の後で all の値が決まっても、条件付きの値は、その接続で先に効く
	got := Collect(writeFiles(t, map[string]string{"etc/ssh/sshd_config": "Match User x\n PasswordAuthentication yes\nMatch all\n PasswordAuthentication no\n"}), true)
	if got.PasswordAuthentication.Value != "no" || !reflect.DeepEqual(got.PasswordAuthentication.MatchValues, []string{"yes"}) {
		t.Errorf("unexpected: %+v", got.PasswordAuthentication)
	}
}

func TestRelativeIncludeAndLiteral(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"etc/ssh/sshd_config":   "Include extra.conf missing.conf conf.d/*.conf\n",
		"etc/ssh/extra.conf":    "PermitRootLogin without-password\n",
		"etc/ssh/conf.d/a.conf": "UsePAM yes\n",
	})
	check(t, Collect(root, true), want{"without-password@/etc/ssh/extra.conf:1", "yes@default", "yes@default", "yes@/etc/ssh/conf.d/a.conf:1"})
}

func TestErrors(t *testing.T) {
	t.Run("not_found", func(t *testing.T) {
		const e = "!not_found:/etc/ssh/sshd_config"
		check(t, Collect(t.TempDir(), true), want{e, e, e, e})
	})

	t.Run("parse_error", func(t *testing.T) {
		// 正しくない値は、その設定だけ判断できない（ほかの設定は読む）
		root := writeFiles(t, map[string]string{"etc/ssh/sshd_config": "PasswordAuthentication maybe\nPermitRootLogin\nUsePAM no\n"})
		check(t, Collect(root, true), want{"!parse_error:/etc/ssh/sshd_config:2", "!parse_error:/etc/ssh/sshd_config:1", "yes@default", "no@/etc/ssh/sshd_config:3"})
	})

	t.Run("too_large", func(t *testing.T) {
		root := writeFiles(t, map[string]string{
			"etc/ssh/sshd_config": "UsePAM yes\nInclude big.conf\n",
			"etc/ssh/big.conf":    strings.Repeat("#\n", maxFileSize),
		})
		// 読めないファイルに "Match all" があるかもしれないため、先に決まった値も判断できない
		const e = "!too_large:/etc/ssh/big.conf"
		check(t, Collect(root, true), want{e, e, e, e})
	})

	t.Run("include_limit", func(t *testing.T) {
		root := writeFiles(t, map[string]string{"etc/ssh/sshd_config": "Include /etc/ssh/sshd_config\n"})
		const e = "!include_limit"
		check(t, Collect(root, true), want{e, e, e, e})
	})
}

// 読めないファイルがあれば、判断できない（値や "Match all" があるかもしれないため）。条件付きの Match の中のファイルは関わらない
func TestPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root は、権限に関わらず読める")
	}

	root := writeFiles(t, map[string]string{
		"etc/ssh/sshd_config":          "PermitRootLogin no\nInclude /etc/ssh/sshd_config.d/*.conf\nInclude /etc/ssh/private.conf\nUsePAM yes\n",
		"etc/ssh/sshd_config.d/a.conf": "Match User foo\nInclude /etc/ssh/match-only.conf\n",
		"etc/ssh/match-only.conf":      "PasswordAuthentication yes\n",
		"etc/ssh/private.conf":         "PasswordAuthentication no\n",
	})
	// 条件付きの Match の中で読むファイルが読めなくても、Match の外の値には関わらない
	os.Chmod(filepath.Join(root, "etc/ssh/match-only.conf"), 0)
	check(t, Collect(root, true), want{"no@/etc/ssh/sshd_config:1", "no@/etc/ssh/private.conf:1", "yes@default", "yes@/etc/ssh/sshd_config:4"})

	os.Chmod(filepath.Join(root, "etc/ssh/private.conf"), 0)
	const e = "!permission_denied:/etc/ssh/private.conf"
	check(t, Collect(root, true), want{e, e, e, e})

	// ディレクトリが読めなければ、中のファイルを知れない
	os.Chmod(filepath.Join(root, "etc/ssh/private.conf"), 0o644)
	os.Chmod(filepath.Join(root, "etc/ssh/sshd_config.d"), 0)
	defer os.Chmod(filepath.Join(root, "etc/ssh/sshd_config.d"), 0o755)
	const d = "!permission_denied:/etc/ssh/sshd_config.d"
	check(t, Collect(root, true), want{d, d, d, d})

	// 設定ファイルそのものが読めない（RHEL系の既定の 0600）
	os.Chmod(filepath.Join(root, "etc/ssh/sshd_config"), 0)
	const m = "!permission_denied:/etc/ssh/sshd_config"
	check(t, Collect(root, true), want{m, m, m, m})
}

func TestNotInstalledJSON(t *testing.T) {
	data, _ := json.Marshal(Collect(t.TempDir(), false))
	if string(data) != `{"installed":false}` {
		t.Fatalf("unexpected: %s", data)
	}

	root := writeFiles(t, map[string]string{"etc/ssh/sshd_config": "PasswordAuthentication no\n"})
	data, _ = json.Marshal(Collect(root, true).PasswordAuthentication)
	if string(data) != `{"value":"no","source":"/etc/ssh/sshd_config:1","match_values":[]}` {
		t.Fatalf("unexpected: %s", data)
	}
	data, _ = json.Marshal(Collect(t.TempDir(), true).UsePAM)
	if string(data) != `{"match_values":[],"error":"not_found:/etc/ssh/sshd_config"}` {
		t.Fatalf("unexpected: %s", data)
	}
}

func TestSplit(t *testing.T) {
	cases := map[string][]string{
		"":                                   nil,
		"   # 注釈":                            nil,
		"PasswordAuthentication no":          {"PasswordAuthentication", "no"},
		"\tPermitRootLogin=yes":              {"PermitRootLogin", "yes"},
		"PermitRootLogin = yes # 注釈":         {"PermitRootLogin", "yes"},
		`Include "/etc/ssh/a b.conf" c.conf`: {"Include", "/etc/ssh/a b.conf", "c.conf"},
		"UsePAM\tyes\r":                      {"UsePAM", "yes"},
		"Match":                              {"Match"},
		"Banner none#x":                      {"Banner", "none#x"},
	}
	for line, want := range cases {
		if got := split(line); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %q（期待 %q）", line, got, want)
		}
	}
}
