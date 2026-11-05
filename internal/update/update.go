// Package update は、エージェントの自動更新。GitHub Releasesから新しい版を取得し、当社の署名（ed25519）を検証できたときだけ置き換える。
//
// リリースには、版と各実行ファイルのSHA-256を並べた checksums.txt と、その署名 checksums.txt.sig（64バイトの生の署名）を添付する。
// checksums.txt の1行目は「version: v1.2.3」で、署名はこの版も含む（古い正しい版を、新しいタグで配り直されても、版で見分けて置き換えない）。
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
	neturl "net/url"
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

// Manifest は、署名を検証したchecksums.txtの内容。
type Manifest struct {
	Version string
	SHA256  string
}

// VerifyChecksums は、checksums.txtの署名を検証し、署名された版と、実行ファイルのSHA-256の値を返す。
func VerifyChecksums(checksums, signature []byte, keys []ed25519.PublicKey, asset string) (*Manifest, error) {
	if len(keys) == 0 {
		return nil, errors.New("検証に使う公開鍵がありません（自動更新しません）")
	}

	verified := false
	for _, key := range keys {
		if ed25519.Verify(key, checksums, signature) {
			verified = true
			break
		}
	}

	if !verified {
		return nil, errors.New("checksums.txtの署名を検証できませんでした")
	}

	manifest := &Manifest{}
	scanner := bufio.NewScanner(strings.NewReader(string(checksums)))
	for scanner.Scan() {
		line := scanner.Text()
		if v, ok := strings.CutPrefix(line, "version: "); ok {
			manifest.Version = strings.TrimSpace(v)
			continue
		}

		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			manifest.SHA256 = strings.ToLower(fields[0])
		}
	}

	switch {
	case manifest.Version == "":
		return nil, errors.New("checksums.txtに、署名された版がありません")
	case manifest.SHA256 == "":
		return nil, fmt.Errorf("checksums.txtに %s がありません", asset)
	}

	return manifest, nil
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

	manifest, err := VerifyChecksums(checksums, signature, PublicKeys(), asset)
	if err != nil {
		return false, err
	}

	// 署名された版が、タグと同じで、今の版より新しいときだけ（古い正しい版への差し戻しを防ぐ）
	if manifest.Version != release.Tag || !Newer(manifest.Version, current) {
		return false, fmt.Errorf("署名された版（%s）が、リリースのタグ（%s）と一致しないか、今の版（%s）より新しくありません", manifest.Version, release.Tag, current)
	}

	binary, err := download(ctx, client, release.Assets[asset], 64<<20)
	if err != nil {
		return false, err
	}

	sum := sha256.Sum256(binary)
	if hex.EncodeToString(sum[:]) != manifest.SHA256 {
		return false, errors.New("実行ファイルのSHA-256が、checksums.txtと一致しません")
	}

	return true, replace(binary, manifest.Version, current)
}

// allowedHosts は、ダウンロードしてよいホスト（https だけ。転送先も含む）。
var allowedHosts = map[string]bool{"github.com": true, "objects.githubusercontent.com": true, "release-assets.githubusercontent.com": true, "api.github.com": true}

// AllowedURL は、ダウンロードしてよいURLか（GitHubのhttpsだけ）。
func AllowedURL(raw string) bool {
	u, err := neturl.Parse(raw)

	return err == nil && u.Scheme == "https" && allowedHosts[u.Hostname()]
}

// HTTPClient は、更新の取得に使うクライアント（転送先もGitHubのhttpsに限る）。
func HTTPClient() *http.Client {
	return &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || !AllowedURL(req.URL.String()) {
				return errors.New("許可していない転送先です")
			}
			return nil
		},
	}
}

func download(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	if url == "" {
		return nil, errors.New("リリースに必要なファイルがありません")
	}

	if !AllowedURL(url) {
		return nil, fmt.Errorf("許可していないURLです: %s", url)
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

// replace は、新しい実行ファイルが動き、署名された版を名乗ることを確かめてから、今の実行ファイルと入れ替える。
// 元の実行ファイルは「.old」として控え、新しい版が送信に成功するまで、更新の途中の印（.update-pending）を置く（`CheckRollback`）。
func replace(binary []byte, want, current string) error {
	self, err := executable()
	if err != nil {
		return err
	}

	next := self + ".new"
	os.Remove(next)
	if err := os.WriteFile(next, binary, 0o755); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, next, "version").Output()
	if err != nil || strings.TrimSpace(string(out)) != want {
		os.Remove(next)
		return fmt.Errorf("新しい版を起動できない、または版が一致しません（置き換えません）: %q %v", strings.TrimSpace(string(out)), err)
	}

	os.Remove(self + ".old")
	if err := os.Link(self, self+".old"); err != nil {
		os.Remove(next)
		return fmt.Errorf("元に戻すための控えを作れませんでした（置き換えません）: %w", err)
	}

	if err := os.WriteFile(self+".update-pending", []byte("0"), 0o644); err != nil {
		return err
	}

	return os.Rename(next, self)
}

func executable() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}

	return filepath.EvalSymlinks(self)
}

// RollbackAttempts は、更新した版が送信に成功しないまま、この回数起動したら、元の版へ戻す。
const RollbackAttempts = 3

// CheckRollback は、起動の最初に呼ぶ。更新の途中で、新しい版が続けて送信に失敗していたら、元の版へ戻す（戻したらtrue。呼び出し側は終了し、systemdが再起動する）。
func CheckRollback() (bool, error) {
	self, err := executable()
	if err != nil {
		return false, err
	}

	data, err := os.ReadFile(self + ".update-pending")
	if err != nil {
		return false, nil
	}

	attempts, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if attempts < RollbackAttempts {
		return false, os.WriteFile(self+".update-pending", []byte(strconv.Itoa(attempts+1)), 0o644)
	}

	if _, err := os.Stat(self + ".old"); err != nil {
		os.Remove(self + ".update-pending")
		return false, errors.New("元の版の控えが無いため、戻せません")
	}

	os.Remove(self + ".update-pending")

	return true, os.Rename(self+".old", self)
}

// ConfirmHealthy は、更新した版が送信に成功したら呼ぶ（更新の途中の印と、元の版の控えを消す）。
func ConfirmHealthy() {
	if self, err := executable(); err == nil {
		if _, err := os.Stat(self + ".update-pending"); err == nil {
			os.Remove(self + ".update-pending")
			os.Remove(self + ".old")
		}
	}
}
