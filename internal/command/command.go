// Package command は、情報の読み取りのための決まったコマンドを、時間と出力の上限を設けて実行する。
// シェルは通さず、実行ファイルは決まったディレクトリからだけ探す（設定やアプリの応答から、任意のコマンドを実行させない）。
package command

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func ownedByRoot(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0
}

// SearchDirs は、実行ファイルを探すディレクトリ（環境変数のPATHは使わない）。システムの場所を先に探す。
var SearchDirs = []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin", "/usr/local/sbin", "/usr/local/bin"}

// RequireRootOwner は、実行ファイルとそのディレクトリが、rootの所有で、他の人が書き換えられないことを求めるか（テストで外す）。
var RequireRootOwner = true

// Options は、実行の上限。
type Options struct {
	// Timeout は、実行の時間の上限（0なら10秒）。
	Timeout time.Duration
	// Limit は、受け取る出力の上限のバイト数（0なら1MiB）。超えた分は捨てる。
	Limit int
	// Stderr は、標準エラー出力も受け取るか（`nginx -v` などは、版を標準エラー出力に出す）。
	Stderr bool
}

// Runner は、コマンドを実行して出力を返す（テストで差し替える）。
type Runner func(ctx context.Context, opts Options, name string, args ...string) ([]byte, error)

// ErrNotFound は、実行ファイルが見つからないとき。
var ErrNotFound = errors.New("実行ファイルが見つかりません")

// ErrUntrusted は、実行ファイルを、root以外が書き換えられるとき（/usr/local を一般のユーザーに渡している環境などで、偽のコマンドを実行しない）。
var ErrUntrusted = errors.New("実行ファイルを、root以外が書き換えられます")

// Trusted は、実行ファイル（シンボリックリンクなら、その先）とそのディレクトリが、rootの所有で、グループや他の人が書けないかを確かめる。
func Trusted(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ErrNotFound
	}

	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return ErrNotFound
	}
	if !RequireRootOwner {
		return nil
	}

	for _, p := range []string{resolved, filepath.Dir(resolved)} {
		info, err := os.Stat(p)
		if err != nil || !ownedByRoot(info) || info.Mode().Perm()&0o022 != 0 {
			return ErrUntrusted
		}
	}

	return nil
}

// LookPath は、名前から実行ファイルを探す（SearchDirsだけを探す）。
func LookPath(name string) (string, error) {
	if strings.ContainsRune(name, '/') {
		return "", ErrNotFound
	}

	for _, dir := range SearchDirs {
		path := filepath.Join(dir, name)
		if Trusted(path) == nil {
			return path, nil
		}
	}

	return "", ErrNotFound
}

// Run は、コマンドを実行する。name は、SearchDirsにある名前か、絶対パス（決まった場所を探して見つけたもの）。
// 終了の状態が0でなくても、それまでの出力を返す。
func Run(ctx context.Context, opts Options, name string, args ...string) ([]byte, error) {
	path := name
	if filepath.IsAbs(name) {
		if err := Trusted(name); err != nil {
			return nil, err
		}
	} else {
		var err error
		if path, err = LookPath(name); err != nil {
			return nil, err
		}
	}

	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.Limit <= 0 {
		opts.Limit = 1 << 20
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	out := &limitedBuffer{limit: opts.Limit}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout = out
	if opts.Stderr {
		cmd.Stderr = out
	}
	// 出力を読み取りやすいよう、言語を固定する。ページャーや色付けもしない
	cmd.Env = []string{"PATH=" + strings.Join(SearchDirs, ":"), "LANG=C", "LC_ALL=C", "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0", "TERM=dumb"}
	cmd.Dir = "/"
	// 子のプロセスが出力を開いたままでも、待ち続けない
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	if ctx.Err() != nil {
		return out.Bytes(), ctx.Err()
	}

	return out.Bytes(), err
}

// limitedBuffer は、上限までだけ溜める（超えた分は捨てるが、書き込みは成功させてコマンドを止めない）。
// bytes.Buffer を埋め込まない（ReadFrom があると、io.Copy が Write を通らず、上限が効かないため）。
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}

	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte {
	return b.buf.Bytes()
}
