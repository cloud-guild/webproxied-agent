// Package metrics は、サーバー監視のメトリクス（CPU・負荷・メモリ・ディスク・プロセス・時刻の同期・OOM）を集める。
// 読み取りだけを行う。/proc などのファイルを読み、決まったコマンド（chronyc・timedatectl・systemctl）だけを実行する。
package metrics

import (
	"bufio"
	"context"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/cloud-guild/webproxied-agent/internal/command"
)

// Metrics は、毎回の送信に加えるメトリクス。読めなかった項目は null にする。
type Metrics struct {
	CPU           *CPU       `json:"cpu"`
	Load          *Load      `json:"load"`
	Memory        *Memory    `json:"memory"`
	Swap          *Swap      `json:"swap"`
	Disks         []Disk     `json:"disks"`
	Processes     *Processes `json:"processes"`
	Time          *TimeSync  `json:"time"`
	OOMKillsTotal *uint64    `json:"oom_kills_total"`
	UptimeSeconds *int64     `json:"uptime_seconds"`
	// ServicesWatched は、アプリが指定したサービスの状態（指定が無ければ送らない）。
	ServicesWatched []WatchedService `json:"services_watched,omitempty"`
	// AccessLog は、アクセスログの件数と5xxの件数（指定が無ければ送らない）。
	AccessLog *AccessLog `json:"access_log,omitempty"`
}

// CPU は、前回の集計から後のCPUの使用率（%）。
type CPU struct {
	UsagePct  float64 `json:"usage_pct"`
	StealPct  float64 `json:"steal_pct"`
	IOWaitPct float64 `json:"iowait_pct"`
}

// Load は、ロードアベレージ（1分・5分・15分）。
type Load struct {
	One     float64 `json:"1"`
	Five    float64 `json:"5"`
	Fifteen float64 `json:"15"`
}

// Memory は、メモリの量。
type Memory struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

// Swap は、スワップの量。
type Swap struct {
	TotalBytes uint64 `json:"total_bytes"`
	UsedBytes  uint64 `json:"used_bytes"`
}

// Processes は、スレッドの数と上限（PIDはスレッドごとに使うため、スレッドの数で数える）。
type Processes struct {
	Count uint64 `json:"count"`
	Max   uint64 `json:"max"`
}

// Collector は、メトリクスを集める（CPUの使用率のため、前回の値を持つ）。1つのgoroutineから使う。
type Collector struct {
	// Root は、/proc を読む場所（テストで差し替える）。
	Root string
	// Run は、コマンドの実行（テストで差し替える）。
	Run command.Runner
	// LookPath は、実行ファイルを探す（テストで差し替える）。
	LookPath func(name string) (string, error)
	// Statfs は、ファイルシステムの容量を調べる（テストで差し替える）。
	Statfs func(path string) (FSStat, error)
	// Now は、今の時刻（テストで差し替える）。
	Now func() time.Time

	prevCPU  *cpuTimes
	disks    diskState
	watch    []string
	logs     *accessLogs
	hasWatch bool
}

// New は、実際のサーバーを読むCollectorを作る。
func New() *Collector {
	c := &Collector{Root: "/", Run: command.Run, LookPath: command.LookPath, Statfs: statfs, Now: time.Now}
	// 最初の送信でもCPUの使用率を出せるよう、今の値を控えておく
	c.prevCPU, _ = c.readCPU()

	return c
}

func (c *Collector) path(name string) string {
	return filepath.Join(c.Root, name)
}

// Collect は、メトリクスを集める。
func (c *Collector) Collect(ctx context.Context) *Metrics {
	m := &Metrics{}

	if now, err := c.readCPU(); err == nil {
		if c.prevCPU != nil {
			m.CPU = cpuUsage(*c.prevCPU, *now)
		}
		c.prevCPU = now
	}

	m.Load, m.Processes = c.readLoad()
	m.Memory, m.Swap = c.readMemory()
	m.Disks = c.collectDisks()
	m.Time = c.timeSync(ctx)
	m.OOMKillsTotal = c.readOOMKills()
	m.UptimeSeconds = c.readUptime()

	if c.hasWatch {
		m.ServicesWatched = c.watchedServices(ctx)
	}
	if c.logs != nil {
		m.AccessLog = c.logs.collect(c.Now())
	}

	return m
}

// Commit は、送信に成功したら呼ぶ（アクセスログの件数を数え直す）。失敗したら呼ばず、次の送信で合わせて送る。
func (c *Collector) Commit() {
	if c.logs != nil {
		c.logs.commit()
	}
}

// cpuTimes は、/proc/stat の cpu の行（単位はUSER_HZ）。
type cpuTimes struct {
	total, idle, iowait, steal uint64
}

func (c *Collector) readCPU() (*cpuTimes, error) {
	f, err := os.Open(c.path("proc/stat"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}

		// user nice system idle iowait irq softirq steal（guestは、userに含まれるため足さない）
		var values [8]uint64
		for i := 0; i < len(values) && i+1 < len(fields); i++ {
			values[i], _ = strconv.ParseUint(fields[i+1], 10, 64)
		}

		t := &cpuTimes{idle: values[3], iowait: values[4], steal: values[7]}
		for _, v := range values {
			t.total += v
		}

		return t, nil
	}

	return nil, os.ErrNotExist
}

// minCPUTicks は、使用率を出すのに要る、前回からのCPUの時間（USER_HZ=100で、CPU1つあたり0.5秒）。短すぎる間隔では、値がぶれるため出さない。
var minCPUTicks = uint64(50 * runtime.NumCPU())

func cpuUsage(prev, now cpuTimes) *CPU {
	if now.total <= prev.total || now.total-prev.total < minCPUTicks {
		return nil
	}

	total := float64(now.total - prev.total)
	delta := func(a, b uint64) float64 {
		if b < a {
			return 0
		}
		return float64(b - a)
	}
	// 使用率には、待ち（idle・iowait）と、他の仮想マシンに取られた時間（steal）を含めない（それぞれ別に送る）
	notUsed := delta(prev.idle, now.idle) + delta(prev.iowait, now.iowait) + delta(prev.steal, now.steal)

	return &CPU{
		UsagePct:  round(max(0, 100*(total-notUsed)/total)),
		StealPct:  round(100 * delta(prev.steal, now.steal) / total),
		IOWaitPct: round(100 * delta(prev.iowait, now.iowait) / total),
	}
}

func round(v float64) float64 {
	return math.Round(v*100) / 100
}

func (c *Collector) readLoad() (*Load, *Processes) {
	data, err := os.ReadFile(c.path("proc/loadavg"))
	if err != nil {
		return nil, nil
	}

	// 例: 0.52 0.58 0.59 2/523 12345（4つ目は、動いている数／スレッドの総数）
	fields := strings.Fields(string(data))
	if len(fields) < 4 {
		return nil, nil
	}

	var load Load
	load.One, _ = strconv.ParseFloat(fields[0], 64)
	load.Five, _ = strconv.ParseFloat(fields[1], 64)
	load.Fifteen, _ = strconv.ParseFloat(fields[2], 64)

	var procs *Processes
	if _, total, ok := strings.Cut(fields[3], "/"); ok {
		if count, err := strconv.ParseUint(total, 10, 64); err == nil {
			procs = &Processes{Count: count}
			// 上限は、PIDの上限とスレッドの上限の小さい方
			for _, name := range []string{"proc/sys/kernel/pid_max", "proc/sys/kernel/threads-max"} {
				if v, ok := c.readUint(name); ok && (procs.Max == 0 || v < procs.Max) {
					procs.Max = v
				}
			}
		}
	}

	return &load, procs
}

func (c *Collector) readUint(name string) (uint64, bool) {
	data, err := os.ReadFile(c.path(name))
	if err != nil {
		return 0, false
	}

	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	return v, err == nil
}

func (c *Collector) readMemory() (*Memory, *Swap) {
	f, err := os.Open(c.path("proc/meminfo"))
	if err != nil {
		return nil, nil
	}
	defer f.Close()

	values := map[string]uint64{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// 例: MemTotal:       16303428 kB
		key, rest, ok := strings.Cut(scanner.Text(), ":")
		fields := strings.Fields(rest)
		if !ok || len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && fields[1] == "kB" {
			v *= 1024
		}
		values[key] = v
	}

	total, ok := values["MemTotal"]
	if !ok {
		return nil, nil
	}

	available, ok := values["MemAvailable"]
	if !ok {
		// MemAvailable の無い古いカーネルでは、空きとキャッシュから見積もる
		available = values["MemFree"] + values["Buffers"] + values["Cached"]
	}

	swap := &Swap{TotalBytes: values["SwapTotal"]}
	if free := values["SwapFree"]; free <= swap.TotalBytes {
		swap.UsedBytes = swap.TotalBytes - free
	}

	return &Memory{TotalBytes: total, AvailableBytes: min(available, total)}, swap
}

func (c *Collector) readOOMKills() *uint64 {
	f, err := os.Open(c.path("proc/vmstat"))
	if err != nil {
		return nil
	}
	defer f.Close()

	// oom_kill は、カーネル4.13以降
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if key, value, ok := strings.Cut(scanner.Text(), " "); ok && key == "oom_kill" {
			if v, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64); err == nil {
				return &v
			}
		}
	}

	return nil
}

func (c *Collector) readUptime() *int64 {
	data, err := os.ReadFile(c.path("proc/uptime"))
	if err != nil {
		return nil
	}

	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return nil
	}

	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return nil
	}

	v := int64(seconds)
	return &v
}
