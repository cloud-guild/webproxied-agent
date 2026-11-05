// webproxied-agent は、WebProxiedのエージェント。サーバーの情報を集め、アプリへHTTPSで送る（読み取りだけを行う）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cloud-guild/webproxied-agent/internal/client"
	"github.com/cloud-guild/webproxied-agent/internal/config"
	"github.com/cloud-guild/webproxied-agent/internal/host"
	"github.com/cloud-guild/webproxied-agent/internal/inventory"
	"github.com/cloud-guild/webproxied-agent/internal/metrics"
	"github.com/cloud-guild/webproxied-agent/internal/update"
)

// version は、ビルド時に埋め込む（`-ldflags "-X main.version=v1.2.3"`）。
var version = "dev"

// updateInterval は、新しい版を確かめる間隔。
const updateInterval = 24 * time.Hour

// minInventoryInterval は、サーバーの情報（inventory）を送る間隔の下限（アプリが続けて求めても、大きなデータを送り続けない）。
const minInventoryInterval = 10 * time.Minute

// maxInventoryBackoff は、サーバーの情報を付けた送信に失敗し続けたときの、付け直すまでの間隔の上限。
const maxInventoryBackoff = 24 * time.Hour

// inventorySchedule は、サーバーの情報を、いつ送るか。起動後の最初の送信と、アプリが求めたとき（応答の inventory_due）に送る。
// 付けた送信に失敗したら（大きすぎる・時間切れなど）、付けずに送り直し、付け直すまでの間隔を延ばす（10分・20分・40分…最大24時間）。
type inventorySchedule struct {
	due     bool
	next    time.Time
	backoff time.Duration
}

func newInventorySchedule() *inventorySchedule {
	return &inventorySchedule{due: true}
}

// attach は、次の送信に付けるか。
func (s *inventorySchedule) attach(now time.Time) bool {
	return s.due && !now.Before(s.next)
}

// sent は、付けた送信に成功したとき。
func (s *inventorySchedule) sent(now time.Time) {
	s.due, s.backoff, s.next = false, 0, now.Add(minInventoryInterval)
}

// failed は、付けた送信に失敗したとき。
func (s *inventorySchedule) failed(now time.Time) {
	s.backoff = min(max(s.backoff*2, minInventoryInterval), maxInventoryBackoff)
	s.next = now.Add(s.backoff)
}

// requested は、アプリが求めたとき（応答の inventory_due）。
func (s *inventorySchedule) requested() {
	s.due = true
}

// Report は、アプリへ送るデータ。
type Report struct {
	AgentVersion string           `json:"agent_version"`
	SentAt       time.Time        `json:"sent_at"`
	Host         host.Info        `json:"host"`
	Metrics      *metrics.Metrics `json:"metrics,omitempty"`
	// Inventory は、起動後の最初の送信と、アプリが求めたとき（応答の inventory_due）だけ送る。
	Inventory *inventory.Inventory `json:"inventory,omitempty"`
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

func newReport(ctx context.Context, mc *metrics.Collector) Report {
	return Report{AgentVersion: version, SentAt: time.Now().UTC(), Host: host.Collect(), Metrics: mc.Collect(ctx)}
}

// once は、1回だけ送る（導入の直後の確認）。
func once(path string) int {
	_, c, ok := load(path)
	if !ok {
		return 1
	}

	ctx := context.Background()
	mc := metrics.New()
	// CPUの使用率を出せるよう、少し間を空けてから集める
	time.Sleep(time.Second)
	res, err := c.Report(ctx, newReport(ctx, mc))
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

	// 更新した版が、続けて送信に失敗していたら、元の版へ戻す
	if rolledBack, err := update.CheckRollback(); err != nil {
		log.Printf("元の版へ戻せませんでした: %v", err)
	} else if rolledBack {
		log.Print("更新した版が送信に成功しないため、元の版へ戻しました。再起動します")
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

	mc := metrics.New()
	ic := inventory.New()
	schedule := newInventorySchedule()

	for {
		select {
		case <-ctx.Done():
			log.Print("終了します")
			return 0
		case <-timer.C:
		}

		rep := newReport(ctx, mc)
		if schedule.attach(time.Now()) {
			inv := ic.Collect(ctx)
			rep.Inventory = &inv
		}

		res, err := c.Report(ctx, rep)
		if err != nil && rep.Inventory != nil && ctx.Err() == nil {
			// 大きなデータが原因で、毎回の送信まで失敗し続けないよう、サーバーの情報を付けずに、すぐ送り直す
			log.Printf("サーバーの情報を付けた送信に失敗しました（%v）。付けずに送り直します", err)
			schedule.failed(time.Now())
			rep.Inventory = nil
			res, err = c.Report(ctx, rep)
		}

		switch {
		case err == nil:
			failures = 0
			update.ConfirmHealthy()
			mc.Commit()
			if rep.Inventory != nil {
				schedule.sent(time.Now())
			}
			if res.InventoryDue {
				schedule.requested()
			}
			applyWatch(mc, res)
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
			if updated, err := update.Apply(ctx, update.HTTPClient(), version); err != nil {
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

// applyWatch は、アプリが指定した、監視するサービスとアクセスログを反映する（応答に無ければ、今の指定のまま）。
func applyWatch(mc *metrics.Collector, res *client.Response) {
	if res.WatchServices != nil {
		mc.SetWatchServices(res.WatchServices)
	}
	if res.AccessLogs != nil {
		specs := make([]metrics.AccessLogSpec, 0, len(res.AccessLogs))
		for _, l := range res.AccessLogs {
			specs = append(specs, metrics.AccessLogSpec{Path: l.Path, Format: l.Format})
		}
		mc.SetAccessLogs(specs)
	}
}

// manualUpdate は、今すぐ新しい版を確かめて更新する。
func manualUpdate() int {
	updated, err := update.Apply(context.Background(), update.HTTPClient(), version)
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
