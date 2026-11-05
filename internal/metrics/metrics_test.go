package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloud-guild/webproxied-agent/internal/command"
)

// testCollector は、/proc をテスト用のディレクトリに差し替えたCollector。
func testCollector(t *testing.T, outputs map[string]string) (*Collector, func(path, content string)) {
	root := t.TempDir()
	write := func(path, content string) {
		full := filepath.Join(root, path)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	c := &Collector{
		Root: root,
		Run: func(_ context.Context, _ command.Options, name string, args ...string) ([]byte, error) {
			if out, ok := outputs[strings.Join(append([]string{name}, args...), " ")]; ok {
				return []byte(out), nil
			}
			return nil, errors.New("failed")
		},
		LookPath: func(name string) (string, error) {
			for key := range outputs {
				if strings.HasPrefix(key, name+" ") {
					return "/usr/bin/" + name, nil
				}
			}
			return "", command.ErrNotFound
		},
		Statfs: func(path string) (FSStat, error) {
			return FSStat{Blocks: 1000, BlocksFree: 400, BlocksAvail: 350, BlockSize: 4096, Files: 100, FilesFree: 60}, nil
		},
		Now: time.Now,
	}

	return c, write
}

func TestCollect(t *testing.T) {
	c, write := testCollector(t, map[string]string{
		"chronyc -n tracking": "Reference ID    : A9FEA97B (169.254.169.123)\nSystem time     : 0.000250000 seconds slow of NTP time\nLeap status     : Normal\n",
	})
	write("proc/stat", "cpu  100 0 100 700 50 0 0 50 0 0\ncpu0 1 2 3\n")
	write("proc/loadavg", "0.52 0.58 0.59 2/523 12345\n")
	write("proc/sys/kernel/pid_max", "4194304\n")
	write("proc/sys/kernel/threads-max", "126738\n")
	write("proc/meminfo", "MemTotal:       16000 kB\nMemFree:  1000 kB\nMemAvailable:   8000 kB\nSwapTotal:  2000 kB\nSwapFree:  1500 kB\n")
	write("proc/vmstat", "nr_free_pages 1\noom_kill 3\n")
	write("proc/uptime", "3600.50 100.00\n")
	write("proc/self/mountinfo", "22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,errors=remount-ro\n"+
		"23 22 0:21 / /run rw - tmpfs tmpfs rw\n")

	// 最初の値を控えてから、CPUを使った分だけ進める
	c.prevCPU, _ = c.readCPU()
	write("proc/stat", "cpu  160 0 140 780 60 0 0 60 0 0\n")

	m := c.Collect(context.Background())

	// 差: total 200、idle 80 + iowait 10 → 使用率 55%、steal 5%、iowait 5%
	if m.CPU == nil || m.CPU.UsagePct != 55 || m.CPU.StealPct != 5 || m.CPU.IOWaitPct != 5 {
		t.Errorf("cpu: %+v", m.CPU)
	}
	if m.Load == nil || m.Load.One != 0.52 || m.Load.Fifteen != 0.59 {
		t.Errorf("load: %+v", m.Load)
	}
	if m.Processes == nil || m.Processes.Count != 523 || m.Processes.Max != 126738 {
		t.Errorf("processes: %+v", m.Processes)
	}
	if m.Memory == nil || m.Memory.TotalBytes != 16000*1024 || m.Memory.AvailableBytes != 8000*1024 {
		t.Errorf("memory: %+v", m.Memory)
	}
	if m.Swap == nil || m.Swap.TotalBytes != 2000*1024 || m.Swap.UsedBytes != 500*1024 {
		t.Errorf("swap: %+v", m.Swap)
	}
	if len(m.Disks) != 1 || m.Disks[0].Mount != "/" || m.Disks[0].UsedBytes != 600*4096 || m.Disks[0].ReadOnly {
		t.Errorf("disks: %+v", m.Disks)
	}
	if m.Time == nil || !m.Time.Synced || m.Time.OffsetMS == nil || *m.Time.OffsetMS != -0.25 {
		t.Errorf("time: %+v", m.Time)
	}
	if m.OOMKillsTotal == nil || *m.OOMKillsTotal != 3 || m.UptimeSeconds == nil || *m.UptimeSeconds != 3600 {
		t.Errorf("oom/uptime: %v %v", m.OOMKillsTotal, m.UptimeSeconds)
	}

	// 監視の指定が無ければ、services_watched・access_log は送らない
	data, _ := json.Marshal(m)
	for _, key := range []string{`"services_watched"`, `"access_log"`} {
		if strings.Contains(string(data), key) {
			t.Errorf("指定が無いのに %s を送る: %s", key, data)
		}
	}
	for _, key := range []string{`"load":{"1":0.52,"5":0.58,"15":0.59}`, `"oom_kills_total":3`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("%s が無い: %s", key, data)
		}
	}
}

func TestCollectMissing(t *testing.T) {
	c, _ := testCollector(t, nil)
	m := c.Collect(context.Background())

	data, _ := json.Marshal(m)
	want := `{"cpu":null,"load":null,"memory":null,"swap":null,"disks":[],"processes":null,"time":null,"oom_kills_total":null,"uptime_seconds":null}`
	if string(data) != want {
		t.Fatalf("読めない項目は null にする: %s", data)
	}
}

func TestCPUCounterReset(t *testing.T) {
	if cpuUsage(cpuTimes{total: 100}, cpuTimes{total: 100}) != nil {
		t.Fatal("差が無いのに使用率を出す")
	}
}

func TestDiskMounts(t *testing.T) {
	info := strings.Join([]string{
		`22 1 8:1 / / ro,relatime shared:1 - ext4 /dev/sda1 rw,errors=remount-ro`,
		`23 22 8:2 / /home rw - ext4 /dev/sda2 rw`,
		`24 22 8:3 / /mnt/my\040data rw - xfs /dev/sda3 ro`,
		`25 22 7:0 / /snap/core/1 ro - squashfs /dev/loop0 ro`,
		`26 22 0:5 / /proc rw - proc proc rw`,
		`27 22 8:1 /var/lib/docker /var/lib/docker rw - ext4 /dev/sda1 rw`,
		// systemd の ProtectHome で /home の上に重なったもの
		`28 23 0:21 /systemd/inaccessible/dir /home ro - tmpfs tmpfs rw`,
	}, "\n")

	mounts := diskMounts(ParseMountinfo(strings.NewReader(info)))
	if len(mounts) != 2 {
		t.Fatalf("unexpected: %+v", mounts)
	}
	// ProtectSystem でマウントが読み取り専用でも、ファイルシステムが書き込めるなら readonly にしない
	if mounts[0].mount != "/" || mounts[0].readOnly {
		t.Errorf("root: %+v", mounts[0])
	}
	if mounts[1].mount != "/mnt/my data" || mounts[1].fstype != "xfs" || !mounts[1].readOnly {
		t.Errorf("escaped: %+v", mounts[1])
	}
}

func TestStatfsTimeout(t *testing.T) {
	c, write := testCollector(t, nil)
	write("proc/self/mountinfo", "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n23 22 0:50 / /mnt/nfs rw - nfs4 server:/x rw\n")

	release := make(chan struct{})
	var calls atomic.Int32
	c.Statfs = func(path string) (FSStat, error) {
		if path == "/mnt/nfs" {
			calls.Add(1)
			<-release
		}
		return FSStat{Blocks: 10, BlockSize: 1}, nil
	}

	start := time.Now()
	if disks := c.collectDisks(); len(disks) != 1 || disks[0].Mount != "/" {
		t.Fatalf("unexpected: %+v", disks)
	}
	if time.Since(start) > statfsTimeout+time.Second {
		t.Fatal("戻らないstatfsを待ち続ける")
	}

	// 戻るまでは、もう一度は調べない（goroutineを増やさない）
	c.collectDisks()
	if n := calls.Load(); n != 1 {
		t.Fatalf("戻らないマウント先を、また調べた: %d", n)
	}

	close(release)
	time.Sleep(50 * time.Millisecond)
	if disks := c.collectDisks(); len(disks) != 2 {
		t.Fatalf("戻ったあとも調べない: %+v", disks)
	}
}

func TestTimeSyncFallback(t *testing.T) {
	c, _ := testCollector(t, map[string]string{"timedatectl show --property=NTPSynchronized --value": "no\n"})
	if ts := c.timeSync(context.Background()); ts == nil || ts.Synced || ts.OffsetMS != nil || ts.Source != "timedatectl" {
		t.Fatalf("unexpected: %+v", ts)
	}

	c, _ = testCollector(t, map[string]string{"timedatectl status": "      Local time: Thu\n    NTP synchronized: yes\n"})
	if ts := c.timeSync(context.Background()); ts == nil || !ts.Synced {
		t.Fatalf("unexpected: %+v", ts)
	}
}

func TestParseChronyTracking(t *testing.T) {
	ts := ParseChronyTracking([]byte("System time     : 0.001500000 seconds fast of NTP time\nLeap status     : Not synchronised\n"))
	if ts == nil || ts.Synced || *ts.OffsetMS != 1.5 {
		t.Fatalf("unexpected: %+v", ts)
	}
	if ParseChronyTracking([]byte("506 Cannot talk to daemon\n")) != nil {
		t.Fatal("読めない出力から、状態を作る")
	}
}

func TestWatchServices(t *testing.T) {
	c, _ := testCollector(t, map[string]string{
		"systemctl show --no-pager --property=LoadState,ActiveState,SubState -- nginx.service php8.3-fpm.service nothere.service": "ActiveState=active\nSubState=running\nLoadState=loaded\n\n" +
			"LoadState=loaded\nActiveState=failed\nSubState=failed\n\nLoadState=not-found\nActiveState=inactive\nSubState=dead\n",
	})

	names := []string{"nginx.service", "php8.3-fpm", "--version", "a;rm -rf /", "../x", "nothere", "nginx"}
	c.SetWatchServices(names)
	got := c.watchedServices(context.Background())

	want := []WatchedService{
		{Name: "nginx", Active: true, LoadState: "loaded", ActiveState: "active", SubState: "running"},
		{Name: "php8.3-fpm", LoadState: "loaded", ActiveState: "failed", SubState: "failed"},
		{Name: "nothere", LoadState: "not-found", ActiveState: "inactive", SubState: "dead"},
	}
	if len(got) != len(want) {
		t.Fatalf("unexpected: %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %+v", i, got[i])
		}
	}

	many := make([]string, 30)
	for i := range many {
		many[i] = "svc" + string(rune('a'+i))
	}
	c.SetWatchServices(many)
	if len(c.watch) != MaxWatchServices {
		t.Fatalf("上限を超えて監視する: %d", len(c.watch))
	}

	c.SetWatchServices([]string{})
	if c.hasWatch {
		t.Fatal("空の指定で、監視をやめない")
	}
}

func TestWatchServicesFailure(t *testing.T) {
	c, _ := testCollector(t, nil)
	c.SetWatchServices([]string{"nginx"})
	got := c.watchedServices(context.Background())
	if len(got) != 1 || got[0].Active || got[0].ActiveState != "unknown" {
		t.Fatalf("unexpected: %+v", got)
	}
}
