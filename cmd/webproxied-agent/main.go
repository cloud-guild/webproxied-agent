// webproxied-agent は、WebProxiedのエージェント。サーバーの情報を集め、アプリへHTTPSで送る（読み取りだけを行う）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cloud-guild/webproxied-agent/internal/client"
	"github.com/cloud-guild/webproxied-agent/internal/config"
	"github.com/cloud-guild/webproxied-agent/internal/host"
	"github.com/cloud-guild/webproxied-agent/internal/update"
)

// version は、ビルド時に埋め込む（`-ldflags "-X main.version=v1.2.3"`）。
var version = "dev"

// updateInterval は、新しい版を確かめる間隔。
const updateInterval = 24 * time.Hour

// Report は、アプリへ送るデータ。
type Report struct {
	AgentVersion string    `json:"agent_version"`
	SentAt       time.Time `json:"sent_at"`
	Host         host.Info `json:"host"`
}

func main() {
	log.SetFlags(0)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	flags := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	configPath := flags.String("config", config.DefaultPath, "設定ファイル")
	_ = flags.Parse(os.Args[2:])

	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "run":
		os.Exit(run(*configPath))
	case "once":
		os.Exit(once(*configPath))
	case "update":
		os.Exit(manualUpdate())
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "使い方: webproxied-agent run|once|update|version [-config /etc/webproxied-agent/agent.conf]")
}

func load(path string) (*config.Config, *client.Client, bool) {
	cfg, err := config.Load(path)
	if err != nil {
		log.Printf("設定を読めませんでした: %v", err)
		return nil, nil, false
	}

	return cfg, client.New(cfg.APIURL, cfg.Token, "webproxied-agent/"+version), true
}

func report(ctx context.Context, c *client.Client) (*client.Response, error) {
	return c.Report(ctx, Report{AgentVersion: version, SentAt: time.Now().UTC(), Host: host.Collect()})
}

// once は、1回だけ送る（導入の直後の確認）。
func once(path string) int {
	_, c, ok := load(path)
	if !ok {
		return 1
	}

	res, err := report(context.Background(), c)
	if err != nil {
		log.Printf("送れませんでした: %v", err)
		return 1
	}

	log.Printf("送りました（サーバーID: %d）", res.ServerID)
	return 0
}

// run は、決めた間隔で送り続ける（systemdのサービスとして動かす）。
func run(path string) int {
	cfg, c, ok := load(path)
	if !ok {
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("webproxied-agent %s を起動しました（%s へ、%s ごとに送ります）", version, cfg.APIURL, cfg.Interval)

	interval := cfg.Interval
	autoUpdate := cfg.AutoUpdate
	// 多数のサーバーが同時に送らないよう、最初の送信を少しずらす
	timer := time.NewTimer(time.Duration(rand.Int64N(int64(10 * time.Second))))
	lastUpdateCheck := time.Now().Add(-updateInterval + time.Duration(rand.Int64N(int64(time.Hour))))
	failures := 0

	for {
		select {
		case <-ctx.Done():
			log.Print("終了します")
			return 0
		case <-timer.C:
		}

		res, err := report(ctx, c)

		switch {
		case err == nil:
			failures = 0
			if res.IntervalSeconds >= 30 {
				interval = time.Duration(res.IntervalSeconds) * time.Second
			}
			if res.AutoUpdate != nil {
				// アプリの画面で自動更新をオフにしたら、それに従う（設定ファイルでオフにしていれば、オフのまま）
				autoUpdate = cfg.AutoUpdate && *res.AutoUpdate
			}
		default:
			log.Printf("送れませんでした: %v", err)
			failures++
		}

		if autoUpdate && time.Since(lastUpdateCheck) >= updateInterval {
			lastUpdateCheck = time.Now()
			if updated, err := update.Apply(ctx, &http.Client{Timeout: 2 * time.Minute}, version); err != nil {
				log.Printf("自動更新しませんでした: %v", err)
			} else if updated {
				log.Print("新しい版へ更新しました。再起動します")
				return 0
			}
		}

		// 送れない間は、間隔を延ばす（最大10分）
		wait := interval
		if failures > 0 {
			wait = min(interval*time.Duration(1<<min(failures, 4)), 10*time.Minute)
		}
		timer.Reset(wait)
	}
}

// manualUpdate は、今すぐ新しい版を確かめて更新する。
func manualUpdate() int {
	updated, err := update.Apply(context.Background(), &http.Client{Timeout: 2 * time.Minute}, version)
	switch {
	case err != nil:
		log.Printf("更新しませんでした: %v", err)
		return 1
	case updated:
		log.Print("新しい版へ更新しました。`systemctl restart webproxied-agent` で再起動してください")
	default:
		log.Print("最新の版です")
	}

	return 0
}
