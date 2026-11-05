package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"regexp"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v1.2.0", "v1.1.9", true}, {"v1.1.9", "v1.2.0", false}, {"v1.2.0", "v1.2.0", false}, {"v2.0.0", "dev", false}, {"v1.10.0", "v1.9.0", true},
	}

	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestVerifyChecksums(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	sum := sha256.Sum256([]byte("binary"))
	checksums := []byte(fmt.Sprintf("version: v1.2.3\n%s  webproxied-agent_linux_amd64\n", hex.EncodeToString(sum[:])))
	signature := ed25519.Sign(private, checksums)

	got, err := VerifyChecksums(checksums, signature, []ed25519.PublicKey{other, public}, "webproxied-agent_linux_amd64")
	if err != nil || got.SHA256 != hex.EncodeToString(sum[:]) || got.Version != "v1.2.3" {
		t.Fatalf("unexpected: %+v %v", got, err)
	}

	unversioned := []byte(fmt.Sprintf("%s  webproxied-agent_linux_amd64\n", hex.EncodeToString(sum[:])))
	if _, err := VerifyChecksums(unversioned, ed25519.Sign(private, unversioned), []ed25519.PublicKey{public}, "webproxied-agent_linux_amd64"); err == nil {
		t.Fatal("版の無い一覧を受け付けてしまう")
	}

	if _, err := VerifyChecksums(checksums, signature, []ed25519.PublicKey{other}, "webproxied-agent_linux_amd64"); err == nil {
		t.Fatal("別の鍵で検証できてしまう")
	}

	if _, err := VerifyChecksums(append(checksums, 'x'), signature, []ed25519.PublicKey{public}, "webproxied-agent_linux_amd64"); err == nil {
		t.Fatal("改ざんした一覧を検証できてしまう")
	}

	if _, err := VerifyChecksums(checksums, signature, nil, "webproxied-agent_linux_amd64"); err == nil {
		t.Fatal("鍵が無いのに検証できてしまう")
	}
}

func TestAllowedURL(t *testing.T) {
	for url, want := range map[string]bool{
		"https://github.com/cloud-guild/webproxied-agent/releases/download/v1/x": true,
		"https://objects.githubusercontent.com/x":                                true,
		"http://github.com/x":               false,
		"https://169.254.169.254/latest":    false,
		"https://github.com.evil.example/x": false,
	} {
		if AllowedURL(url) != want {
			t.Errorf("AllowedURL(%s) != %v", url, want)
		}
	}
}

func TestPublicKeysFromExtra(t *testing.T) {
	embedded := len(PublicKeys())

	public, _, _ := ed25519.GenerateKey(rand.Reader)
	ExtraKeys = base64.StdEncoding.EncodeToString(public) + ",invalid"
	defer func() { ExtraKeys = "" }()

	// 埋め込みの鍵の後ろに、正しい鍵だけが加わる
	if keys := PublicKeys(); len(keys) != embedded+1 || !keys[embedded].Equal(public) {
		t.Fatalf("unexpected keys: %v", keys)
	}
}

// keys/*.pub の鍵は1つか2つで、install.sh の PUBLIC_KEY_PEM の鍵は、その中にある（片方だけ入れ替えるのを防ぐ）。
func TestEmbeddedKeysMatchInstallScript(t *testing.T) {
	keys := PublicKeys()
	if len(keys) < 1 || len(keys) > 2 {
		t.Fatalf("keys/*.pub の鍵の数が正しくありません: %d", len(keys))
	}

	script, err := os.ReadFile("../../packaging/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)\nPUBLIC_KEY_PEM="([^"]*)"`).FindSubmatch(script)
	if m == nil {
		t.Fatal("install.sh に PUBLIC_KEY_PEM がありません")
	}
	block, _ := pem.Decode(m[1])
	if block == nil || block.Type != "PUBLIC KEY" {
		t.Fatal("install.sh の PUBLIC_KEY_PEM を読めません")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	public, ok := parsed.(ed25519.PublicKey)
	if err != nil || !ok {
		t.Fatalf("install.sh の PUBLIC_KEY_PEM が ed25519 の公開鍵ではありません: %v", err)
	}

	for _, key := range keys {
		if key.Equal(public) {
			return
		}
	}
	t.Fatal("install.sh の PUBLIC_KEY_PEM が、keys/*.pub にありません")
}
