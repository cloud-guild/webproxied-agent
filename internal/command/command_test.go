package command

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunLimitsOutput(t *testing.T) {
	out, err := Run(context.Background(), Options{Limit: 10}, "sh", "-c", "printf '%0100d' 0")
	if err != nil || len(out) != 10 {
		t.Fatalf("unexpected: %q %v", out, err)
	}
}

func TestRunStderr(t *testing.T) {
	out, _ := Run(context.Background(), Options{Stderr: true}, "sh", "-c", "echo v1 >&2")
	if string(out) != "v1\n" {
		t.Fatalf("unexpected: %q", out)
	}

	out, _ = Run(context.Background(), Options{}, "sh", "-c", "echo v1 >&2")
	if len(out) != 0 {
		t.Fatalf("標準エラー出力を受け取ってしまう: %q", out)
	}
}

func TestRunTimeout(t *testing.T) {
	start := time.Now()
	_, err := Run(context.Background(), Options{Timeout: 200 * time.Millisecond}, "sleep", "5")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("時間の上限で止まらない: %v %s", err, time.Since(start))
	}
}

func TestRunEnvironment(t *testing.T) {
	t.Setenv("WEBPROXIED_SECRET", "x")
	out, _ := Run(context.Background(), Options{}, "sh", "-c", "echo \"${WEBPROXIED_SECRET:-none} $LC_ALL\"")
	if string(out) != "none C\n" {
		t.Fatalf("環境変数を引き継いでしまう: %q", out)
	}
}

func TestTrusted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tool")
	os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755)
	os.Chmod(dir, 0o755)

	if os.Getuid() == 0 {
		if err := Trusted(path); err != nil {
			t.Fatalf("rootの実行ファイルを信頼しない: %v", err)
		}
	}

	// 他の人が書けるディレクトリ・実行ファイル
	os.Chmod(dir, 0o777)
	if err := Trusted(path); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("他の人が書けるディレクトリを信頼する: %v", err)
	}
	os.Chmod(dir, 0o755)
	os.Chmod(path, 0o777)
	if err := Trusted(path); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("他の人が書ける実行ファイルを信頼する: %v", err)
	}

	// root以外の所有
	os.Chmod(path, 0o755)
	if os.Getuid() == 0 {
		os.Chown(path, 1000, 1000)
		if err := Trusted(path); !errors.Is(err, ErrUntrusted) {
			t.Fatalf("root以外の所有を信頼する: %v", err)
		}
		if _, err := Run(context.Background(), Options{}, path); !errors.Is(err, ErrUntrusted) {
			t.Fatalf("絶対パスでも確かめない: %v", err)
		}
	}
}

func TestLookPathRejectsPaths(t *testing.T) {
	if _, err := LookPath("../bin/sh"); err == nil {
		t.Fatal("パスを含む名前を受け付けてしまう")
	}
	if _, err := Run(context.Background(), Options{}, "no-such-command-webproxied"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unexpected: %v", err)
	}
}
