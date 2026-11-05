// Package update は、エージェントの自動更新。GitHub Releasesから新しい版を取得し、当社の署名（ed25519）を検証できたときだけ置き換える。
//
// リリースには、各実行ファイルのSHA-256を並べた checksums.txt と、その署名 checksums.txt.sig（64バイトの生の署名）を添付する。
// 署名は、配布元（GitHub）とは別の場所で保管する秘密鍵で行うため、配布物が差し替えられても、検証に失敗して置き換えない。
package update

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// keysFS は、検証に使う公開鍵（keys/*.pub。1行に1つ、32バイトの公開鍵をbase64で）。鍵の入れ替えに備え、2つまで置ける。
//
//go:embed keys
var keysFS embed.FS

// ExtraKeys は、ビルド時に加える公開鍵（開発用。`-ldflags "-X .../update.ExtraKeys=<base64>"`）。本番のビルドでは空。
var ExtraKeys string

// ReleasesURL は、最新のリリースを確かめるAPI。
var ReleasesURL = "https://api.github.com/repos/cloud-guild/webproxied-agent/releases/latest"

// Release は、最新のリリース。
type Release struct {
	Tag    string
	Assets map[string]string
}

// PublicKeys は、検証に使う公開鍵。
func PublicKeys() []ed25519.PublicKey {
	var lines []string

	entries, _ := keysFS.ReadDir("keys")
	for _, entry := range entries {
		if data, err := keysFS.ReadFile("keys/" + entry.Name()); err == nil && strings.HasSuffix(entry.Name(), ".pub") {
			lines = append(lines, strings.Split(string(data), "\n")...)
		}
	}

	lines = append(lines, strings.Split(ExtraKeys, ",")...)

	var keys []ed25519.PublicKey
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if raw, err := base64.StdEncoding.DecodeString(line); err == nil && len(raw) == ed25519.PublicKeySize {
			keys = append(keys, ed25519.PublicKey(raw))
		}
	}

	return keys
}

// AssetName は、このOS・CPUの実行ファイルの名前。
func AssetName() string {
	return fmt.Sprintf("webproxied-agent_%s_%s", runtime.GOOS, runtime.GOARCH)
}

// Latest は、最新のリリースを取得する。
func Latest(ctx context.Context, client *http.Client) (*Release, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ReleasesURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("リリースを取得できませんでした（%d）", res.StatusCode)
	}

	var body struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&body); err != nil {
		return nil, err
	}

	release := &Release{Tag: body.TagName, Assets: map[string]string{}}
	for _, asset := range body.Assets {
		release.Assets[asset.Name] = asset.URL
	}

	return release, nil
}

// Newer は、aがbより新しい版か（v1.2.3の形。比べられなければfalse）。
func Newer(a, b string) bool {
	pa, okA := parse(a)
	pb, okB := parse(b)
	if !okA || !okB {
		return false
	}

	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}

	return false
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) != 3 {
		return out, false
	}

	for i, part := range parts {
		n, err := strconv.Atoi(strings.SplitN(part, "-", 2)[0])
		if err != nil {
			return out, false
		}
		out[i] = n
	}

	return out, true
}

// VerifyChecksums は、checksums.txtの署名を検証し、実行ファイルのSHA-256の値を返す。
func VerifyChecksums(checksums, signature []byte, keys []ed25519.PublicKey, asset string) (string, error) {
	if len(keys) == 0 {
		return "", errors.New("検証に使う公開鍵がありません（自動更新しません）")
	}

	verified := false
	for _, key := range keys {
		if ed25519.Verify(key, checksums, signature) {
			verified = true
			break
		}
	}

	if !verified {
		return "", errors.New("checksums.txtの署名を検証できませんでした")
	}

	scanner := bufio.NewScanner(strings.NewReader(string(checksums)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			return strings.ToLower(fields[0]), nil
		}
	}

	return "", fmt.Errorf("checksums.txtに %s がありません", asset)
}

// Apply は、最新の版を取得・検証し、今の実行ファイルを置き換える。置き換えたらtrue（呼び出し側は終了し、systemdが再起動する）。
func Apply(ctx context.Context, client *http.Client, current string) (bool, error) {
	release, err := Latest(ctx, client)
	if err != nil || !Newer(release.Tag, current) {
		return false, err
	}

	asset := AssetName()
	checksums, err := download(ctx, client, release.Assets["checksums.txt"], 1<<20)
	if err != nil {
		return false, err
	}

	signature, err := download(ctx, client, release.Assets["checksums.txt.sig"], 1<<10)
	if err != nil {
		return false, err
	}

	want, err := VerifyChecksums(checksums, signature, PublicKeys(), asset)
	if err != nil {
		return false, err
	}

	binary, err := download(ctx, client, release.Assets[asset], 64<<20)
	if err != nil {
		return false, err
	}

	sum := sha256.Sum256(binary)
	if hex.EncodeToString(sum[:]) != want {
		return false, errors.New("実行ファイルのSHA-256が、checksums.txtと一致しません")
	}

	return true, replace(binary)
}

func download(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	if url == "" {
		return nil, errors.New("リリースに必要なファイルがありません")
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ダウンロードできませんでした（%d）", res.StatusCode)
	}

	return io.ReadAll(io.LimitReader(res.Body, limit))
}

// replace は、新しい実行ファイルが動くかを確かめてから、今の実行ファイルと入れ替える（同じディレクトリに書いてから名前を変える）。
func replace(binary []byte) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}

	self, _ = filepath.EvalSymlinks(self)
	next := self + ".new"
	if err := os.WriteFile(next, binary, 0o755); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := exec.CommandContext(ctx, next, "version").Run(); err != nil {
		os.Remove(next)
		return fmt.Errorf("新しい版を起動できませんでした（置き換えません）: %w", err)
	}

	os.Remove(self + ".old")
	if err := os.Link(self, self+".old"); err != nil {
		_ = err // 元に戻すための控え。作れなくても更新は続ける
	}

	return os.Rename(next, self)
}
