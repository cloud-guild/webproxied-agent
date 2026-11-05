package metrics

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

// Disk は、ファイルシステムの容量。
type Disk struct {
	Mount  string `json:"mount"`
	FSType string `json:"fstype"`
	// TotalBytes・UsedBytes・AvailableBytes は、dfと同じ数え方（AvailableBytesは、一般のユーザーが使える空き）。
	TotalBytes     uint64 `json:"total_bytes"`
	UsedBytes      uint64 `json:"used_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	InodesTotal    uint64 `json:"inodes_total"`
	InodesFree     uint64 `json:"inodes_free"`
	// ReadOnly は、ファイルシステムが読み取り専用か（エラーで読み取り専用に切り替わったときなど）。
	ReadOnly bool `json:"readonly"`
}

// FSStat は、statfs の結果。
type FSStat struct {
	Blocks, BlocksFree, BlocksAvail uint64
	BlockSize                       uint64
	Files, FilesFree                uint64
}

// statfsTimeout は、statfs を待つ時間（応答の無いNFSなどで止まらないよう）。
const statfsTimeout = 2 * time.Second

// excludedFS は、容量を送らないファイルシステム（メモリ上・カーネル・読み取り専用のイメージ・コンテナの重ね合わせ）。
var excludedFS = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "ramfs": true, "overlay": true, "aufs": true, "squashfs": true, "iso9660": true,
	"proc": true, "sysfs": true, "cgroup": true, "cgroup2": true, "devpts": true, "mqueue": true, "debugfs": true,
	"tracefs": true, "securityfs": true, "pstore": true, "bpf": true, "autofs": true, "hugetlbfs": true, "configfs": true,
	"fusectl": true, "binfmt_misc": true, "rpc_pipefs": true, "nsfs": true, "efivarfs": true, "selinuxfs": true,
	"nfsd": true, "fuse.lxcfs": true, "fuse.snapfuse": true, "fuse.gvfsd-fuse": true, "fuse.portal": true, "zram": true,
}

// mountEntry は、/proc/self/mountinfo の1行。
type mountEntry struct {
	device   string
	mount    string
	fstype   string
	readOnly bool
}

// ParseMountinfo は、/proc/self/mountinfo を読む。
// 形式: ID 親ID major:minor root マウント先 マウントのオプション [任意の項目...] - 種類 元 スーパーブロックのオプション
func ParseMountinfo(r interface{ Read([]byte) (int, error) }) []mountEntry {
	var entries []mountEntry

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(fields) {
			continue
		}

		entry := mountEntry{device: fields[2], mount: unescapeMount(fields[4]), fstype: fields[sep+1]}
		// 読み取り専用かは、スーパーブロックのオプションで見る（systemdのProtectSystemは、マウントのオプションだけを読み取り専用にするため）
		if sep+3 < len(fields) {
			for _, opt := range strings.Split(fields[sep+3], ",") {
				if opt == "ro" {
					entry.readOnly = true
				}
			}
		}
		entries = append(entries, entry)
	}

	return entries
}

// unescapeMount は、マウント先の \040（空白）などを戻す。
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}

	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}

	return b.String()
}

// diskMounts は、容量を送るマウント先を選ぶ。
// 同じマウント先に重ねてマウントされていれば、いちばん上だけを見る（statfsは、いちばん上を調べるため。
// systemdのProtectHomeで隠された /home などは、上に重なったtmpfsを除くと、送らない）。同じデバイスは1回だけ送る。
func diskMounts(entries []mountEntry) []mountEntry {
	top := map[string]int{}
	for i, e := range entries {
		top[e.mount] = i
	}

	var out []mountEntry
	for i, e := range entries {
		if top[e.mount] != i || excludedFS[e.fstype] {
			continue
		}
		if e.mount == "/proc" || strings.HasPrefix(e.mount, "/proc/") || strings.HasPrefix(e.mount, "/sys/") || strings.HasPrefix(e.mount, "/dev/") {
			continue
		}
		out = append(out, e)
	}

	return out
}

// diskState は、statfs が戻らないマウント先（応答の無いNFSなど）を覚えておく。
type diskState struct {
	hung map[string]chan statResult
}

type statResult struct {
	stat FSStat
	err  error
}

func (c *Collector) collectDisks() []Disk {
	disks := []Disk{}

	f, err := os.Open(c.path("proc/self/mountinfo"))
	if err != nil {
		return disks
	}
	entries := ParseMountinfo(f)
	f.Close()

	// 同じデバイス（バインドマウント・btrfsのサブボリュームなど）は、調べられた最初のマウント先だけを送る
	devices := map[string]bool{}
	for _, e := range diskMounts(entries) {
		if devices[e.device] {
			continue
		}
		stat, ok := c.statWithTimeout(e.mount)
		if !ok || stat.Blocks == 0 {
			continue
		}
		devices[e.device] = true

		used := uint64(0)
		if stat.Blocks >= stat.BlocksFree {
			used = (stat.Blocks - stat.BlocksFree) * stat.BlockSize
		}
		disks = append(disks, Disk{
			Mount: e.mount, FSType: e.fstype, TotalBytes: stat.Blocks * stat.BlockSize, UsedBytes: used,
			AvailableBytes: stat.BlocksAvail * stat.BlockSize, InodesTotal: stat.Files, InodesFree: stat.FilesFree, ReadOnly: e.readOnly,
		})
	}

	return disks
}

// statWithTimeout は、statfs を時間を限って調べる。戻らなければ、そのマウント先は、戻るまで調べない（goroutineを増やさない）。
func (c *Collector) statWithTimeout(mount string) (FSStat, bool) {
	if c.disks.hung == nil {
		c.disks.hung = map[string]chan statResult{}
	}

	if ch, ok := c.disks.hung[mount]; ok {
		select {
		case <-ch:
			delete(c.disks.hung, mount)
		default:
			return FSStat{}, false
		}
	}

	ch := make(chan statResult, 1)
	go func() {
		stat, err := c.Statfs(mount)
		ch <- statResult{stat, err}
	}()

	select {
	case r := <-ch:
		return r.stat, r.err == nil
	case <-time.After(statfsTimeout):
		c.disks.hung[mount] = ch
		return FSStat{}, false
	}
}
