// Package sshd は、sshd の設定ファイル（/etc/ssh/sshd_config と、Include で読み込むファイル）を読み、
// ログインに関わる設定の、効いている値を調べる。読み取りだけを行う。
// sshd -T は使わない（root が要り、ホスト鍵を読むため）。代わりに、sshd と同じ順で設定ファイルを読む。
//   - 同じ設定は、最初に現れた値が勝つ。
//   - Include は、その場所で展開する（相対パスは /etc/ssh から。glob は名前の順）。
//   - Match の後は、次の Match かファイルの終わりまでが、接続によって効く設定。一致した Match の値が、Match の外の値より優先する。
//     "Match all" は、すべての接続に一致するため、その値が Match の外の値に代わる（sshd -T でも同じ）。
//
// 読めないファイルがあれば、判断できない（そのファイルの中に、値や "Match all" があるかもしれないため）。
// ただし、条件付きの Match の中で読むファイルは、Match の外の値に関わらない。
package sshd

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ConfigPath は、sshd の設定ファイル。
const ConfigPath = "/etc/ssh/sshd_config"

const (
	// maxFileSize は、1つの設定ファイルの大きさの上限。
	maxFileSize = 1 << 20
	// maxDepth は、Include の深さの上限（sshd と同じ）。
	maxDepth = 16
	// maxFiles は、読む設定ファイルの数の上限。
	maxFiles = 100
	// maxString は、値・パスの文字列の長さの上限。
	maxString = 512
)

// Status は、sshd の設定。sshd が入っていなければ、Installed だけ。
type Status struct {
	Installed                    bool     `json:"installed"`
	PermitRootLogin              *Setting `json:"permit_root_login,omitempty"`
	PasswordAuthentication       *Setting `json:"password_authentication,omitempty"`
	KbdInteractiveAuthentication *Setting `json:"kbd_interactive_authentication,omitempty"`
	UsePAM                       *Setting `json:"use_pam,omitempty"`
}

// Setting は、1つの設定の値。判断できなければ、Value の代わりに Error に理由を入れる。
type Setting struct {
	// Value は、Match の外で、最初に現れた値（小文字）。どこにも書かれていなければ、OpenSSH の既定の値。
	Value string `json:"value,omitempty"`
	// Source は、"<ファイル>:<行>"。どこにも書かれていなければ "default"。
	Source string `json:"source,omitempty"`
	// MatchValues は、条件付きの Match の中で設定している値（重複を除き、名前の順）。一致した接続だけ、Value に代わる。
	MatchValues []string `json:"match_values"`
	// Error は、判断できない理由（permission_denied:<ファイル> など）。
	Error string `json:"error,omitempty"`
}

// key は、調べる設定。
type key struct {
	// names は、設定の名前（小文字）。古い名前も同じ設定として読む
	names []string
	// def は、どこにも書かれていないときの、OpenSSH の既定の値
	def string
	// values は、正しい値
	values map[string]bool
	// global は、Match の中で使えない設定（sshd は、Match の中にあると起動しない）
	global bool
}

var yesNo = map[string]bool{"yes": true, "no": true}

var keys = []key{
	{names: []string{"permitrootlogin"}, def: "prohibit-password", values: map[string]bool{"yes": true, "no": true, "prohibit-password": true, "without-password": true, "forced-commands-only": true}},
	{names: []string{"passwordauthentication"}, def: "yes", values: yesNo},
	// ChallengeResponseAuthentication は、KbdInteractiveAuthentication の古い名前（sshd は同じ設定として扱う）
	{names: []string{"kbdinteractiveauthentication", "challengeresponseauthentication"}, def: "yes", values: yesNo},
	{names: []string{"usepam"}, def: "no", values: yesNo, global: true},
}

// context は、読んでいる行が、Match の外か、"Match all" の中か、条件付きの Match の中か。
type context int

const (
	outside context = iota
	matchAll
	matchCond
)

// slot は、Match の外、または "Match all" の中で、最初に現れた値。
type slot struct {
	value, source, err string
	decided            bool
}

// set は、まだ決まっていなければ、値を決める。
func (s *slot) set(value, source string) {
	if !s.decided && s.err == "" {
		s.value, s.source, s.decided = value, source, true
	}
}

// unknown は、まだ決まっていなければ、判断できないことにする。
func (s *slot) unknown(reason string) {
	if !s.decided && s.err == "" {
		s.err = limit(reason)
	}
}

// state は、読んでいる途中の、1つの設定の状態。
type state struct {
	outside, all slot
	match        map[string]bool
}

type reader struct {
	root   string
	files  int
	states []*state
}

// Collect は、sshd の設定を読む。root は、ファイルを読む場所（テストで差し替える。実際のサーバーは "/"）。
func Collect(root string, installed bool) *Status {
	if !installed {
		return &Status{}
	}

	r := &reader{root: root}
	for range keys {
		r.states = append(r.states, &state{match: map[string]bool{}})
	}
	r.file(ConfigPath, 0, outside, true)

	settings := make([]*Setting, len(keys))
	for i, s := range r.states {
		matchValues := []string{}
		for v := range s.match {
			matchValues = append(matchValues, v)
		}
		sort.Strings(matchValues)

		switch {
		case s.all.err != "":
			settings[i] = &Setting{Error: s.all.err, MatchValues: matchValues}
		case s.all.decided:
			settings[i] = &Setting{Value: s.all.value, Source: s.all.source, MatchValues: matchValues}
		case s.outside.err != "":
			settings[i] = &Setting{Error: s.outside.err, MatchValues: matchValues}
		case s.outside.decided:
			settings[i] = &Setting{Value: s.outside.value, Source: s.outside.source, MatchValues: matchValues}
		default:
			settings[i] = &Setting{Value: keys[i].def, Source: "default", MatchValues: matchValues}
		}
	}

	return &Status{Installed: true, PermitRootLogin: settings[0], PasswordAuthentication: settings[1], KbdInteractiveAuthentication: settings[2], UsePAM: settings[3]}
}

// fail は、読めないファイルがあったことを記録する。まだ値が決まっていない設定は、判断できない
// （読めないファイルに、値や "Match all" が書かれているかもしれないため）。条件付きの Match の中なら、関わらない。
func (r *reader) fail(reason string, ctx context) {
	for _, s := range r.states {
		switch ctx {
		case outside:
			s.outside.unknown(reason)
			s.all.unknown(reason)
		case matchAll:
			s.all.unknown(reason)
		}
	}
}

// file は、設定ファイルを1つ読む。Include は、その場所で読む。Match は、ファイルの終わりで終わる（sshd と同じ）。
func (r *reader) file(name string, depth int, ctx context, required bool) {
	r.files++
	if r.files > maxFiles || depth > maxDepth {
		r.fail("include_limit", ctx)
		return
	}

	data, reason := r.read(name, required)
	if reason != "" {
		r.fail(reason, ctx)
		return
	}

	for n, line := range strings.Split(string(data), "\n") {
		words := split(line)
		if len(words) == 0 {
			continue
		}
		keyword, args := strings.ToLower(words[0]), words[1:]

		switch keyword {
		case "match":
			ctx = matchCond
			if len(args) == 1 && strings.EqualFold(args[0], "all") {
				ctx = matchAll
			}
			continue
		case "include":
			for _, pattern := range args {
				paths, reason := r.expand(pattern)
				if reason != "" {
					r.fail(reason, ctx)
					continue
				}
				for _, p := range paths {
					r.file(p, depth+1, ctx, false)
				}
			}
			continue
		}

		for i, k := range keys {
			if !contains(k.names, keyword) {
				continue
			}
			s := r.states[i]
			value := ""
			if len(args) > 0 {
				value = strings.ToLower(args[0])
			}
			if ctx != outside && k.global {
				continue
			}
			target := &s.outside
			switch ctx {
			case matchCond:
				// "Match all" で先に決まっていれば、後の条件付きの Match は効かない
				if k.values[value] && !s.all.decided {
					s.match[value] = true
				}
				continue
			case matchAll:
				target = &s.all
			}
			if !k.values[value] {
				target.unknown("parse_error:" + name + ":" + strconv.Itoa(n+1))
				continue
			}
			target.set(value, limit(name+":"+strconv.Itoa(n+1)))
		}
	}
}

// read は、設定ファイルを読む。required でないファイル（Include で見つけた後に消えたもの）が無ければ、空として扱う。
func (r *reader) read(name string, required bool) ([]byte, string) {
	f, err := os.Open(filepath.Join(r.root, name))
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist) && !required:
			return nil, ""
		case errors.Is(err, fs.ErrNotExist):
			return nil, "not_found:" + name
		case errors.Is(err, fs.ErrPermission):
			return nil, "permission_denied:" + name
		}
		return nil, "read_error:" + name
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, "read_error:" + name
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, "read_error:" + name
	}
	if len(data) > maxFileSize {
		return nil, "too_large:" + name
	}

	return data, ""
}

// expand は、Include のパターンを、ファイルのパス（名前の順）にする。見つからなければ空（sshd も無視する）。
// ディレクトリが読めなければ、あるはずのファイルを読めないため、理由を返す。
func (r *reader) expand(pattern string) ([]string, string) {
	if !strings.HasPrefix(pattern, "/") {
		pattern = "/etc/ssh/" + pattern
	}
	pattern = path.Clean(pattern)
	dir, base := path.Split(pattern)

	if strings.ContainsAny(dir, "*?[") {
		// ディレクトリの部分の glob（めったに無い）。読めないディレクトリは、区別できない
		matches, err := filepath.Glob(filepath.Join(r.root, pattern))
		if err != nil {
			return nil, limit("parse_error:" + pattern)
		}
		var paths []string
		for _, m := range matches {
			if rel, err := filepath.Rel(r.root, m); err == nil {
				paths = append(paths, "/"+filepath.ToSlash(rel))
			}
		}
		sort.Strings(paths)
		return paths, ""
	}

	if !strings.ContainsAny(base, "*?[") {
		if _, err := os.Stat(filepath.Join(r.root, pattern)); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, ""
			}
			if errors.Is(err, fs.ErrPermission) {
				return nil, limit("permission_denied:" + dir)
			}
			return nil, limit("read_error:" + pattern)
		}
		return []string{pattern}, ""
	}

	entries, err := os.ReadDir(filepath.Join(r.root, dir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ""
		}
		if errors.Is(err, fs.ErrPermission) {
			return nil, limit("permission_denied:" + strings.TrimSuffix(dir, "/"))
		}
		return nil, limit("read_error:" + strings.TrimSuffix(dir, "/"))
	}

	var paths []string
	for _, e := range entries {
		name := e.Name()
		// glob と同じく、. で始まる名前は、パターンが . で始まるときだけ
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(base, ".") {
			continue
		}
		if ok, err := path.Match(base, name); err != nil {
			return nil, limit("parse_error:" + pattern)
		} else if ok {
			paths = append(paths, dir+name)
		}
	}
	sort.Strings(paths)

	return paths, ""
}

// split は、設定の1行を、名前と引数に分ける。sshd と同じく、名前と値の間の "=" も区切りとして読み、
// 引数は空白で区切る（"…" で囲んだものは1つ）。# で始まる語から後は注釈。
func split(line string) []string {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return nil
	}

	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return []string{line}
	}
	words := []string{line[:i]}
	rest := strings.TrimLeft(line[i:], " \t")
	if strings.HasPrefix(rest, "=") {
		rest = strings.TrimLeft(rest[1:], " \t")
	}

	for {
		rest = strings.TrimLeft(rest, " \t\r")
		if rest == "" || rest[0] == '#' {
			break
		}
		if rest[0] == '"' {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				words = append(words, rest[1:])
				break
			}
			words = append(words, rest[1:1+end])
			rest = rest[2+end:]
			continue
		}
		end := strings.IndexAny(rest, " \t\r")
		if end < 0 {
			words = append(words, rest)
			break
		}
		words = append(words, rest[:end])
		rest = rest[end:]
	}

	return words
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}

	return false
}

func limit(s string) string {
	if len(s) > maxString {
		return s[:maxString]
	}

	return s
}
