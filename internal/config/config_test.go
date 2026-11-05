package config

import (
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cfg, err := Parse(strings.NewReader("# 注釈\nAPI_URL=https://webproxied.com/\nTOKEN=\"abc\"\nINTERVAL_SECONDS=120\nAUTO_UPDATE=false\n"))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.APIURL != "https://webproxied.com" || cfg.Token != "abc" || cfg.Interval != 2*time.Minute || cfg.AutoUpdate {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestParseDefaultsAndErrors(t *testing.T) {
	cfg, err := Parse(strings.NewReader("API_URL=https://example.com\nTOKEN=t\n"))
	if err != nil || cfg.Interval != time.Minute || !cfg.AutoUpdate {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}

	if _, err := Parse(strings.NewReader("API_URL=http://localhost:8080\nTOKEN=t\nALLOW_INSECURE_HTTP=true\n")); err != nil {
		t.Fatalf("開発用のhttpを許せない: %v", err)
	}

	for _, input := range []string{"API_URL=https://example.com\n", "TOKEN=t\nAPI_URL=http://example.com\n", "TOKEN=t\nAPI_URL=ftp://x\n", "TOKEN=t\nAPI_URL=https://x\nINTERVAL_SECONDS=5\n", "garbage\n"} {
		if _, err := Parse(strings.NewReader(input)); err == nil {
			t.Fatalf("expected error for %q", input)
		}
	}
}

func TestParseIgnoresInstallerKeys(t *testing.T) {
	// 導入スクリプトが書く READ_LOGS は、エージェントでは使わない（入れ直しのときに読む）
	cfg, err := Parse(strings.NewReader("API_URL=http://localhost:8000\nTOKEN=wpa_x\nALLOW_INSECURE_HTTP=true\nREAD_LOGS=true\n"))
	if err != nil || !cfg.AllowInsecureHTTP {
		t.Fatalf("unexpected: %+v %v", cfg, err)
	}
}
