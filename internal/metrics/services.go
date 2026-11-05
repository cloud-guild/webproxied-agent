package metrics

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/cloud-guild/webproxied-agent/internal/command"
)

// MaxWatchServices は、監視するサービスの数の上限。
const MaxWatchServices = 20

// WatchedService は、監視するサービスの状態。
type WatchedService struct {
	Name   string `json:"name"`
	Active bool   `json:"active"`
	// LoadState は、ユニットの読み込みの状態（not-found なら、そのサービスは無い）。
	LoadState string `json:"load_state"`
	// ActiveState・SubState は、systemdの状態（active・failed・inactive など）。
	ActiveState string `json:"active_state"`
	SubState    string `json:"sub_state"`
}

// serviceName は、systemdのユニットの名前に使える文字だけ（先頭の - は、systemctl のオプションと紛れるため許さない）。
var serviceName = regexp.MustCompile(`^[A-Za-z0-9_@:][A-Za-z0-9_@:.\-\\]{0,250}$`)

// ValidServiceName は、アプリが指定したサービスの名前が、ユニットの名前として正しいか。
func ValidServiceName(name string) bool {
	return serviceName.MatchString(name)
}

// SetWatchServices は、監視するサービスを決める（アプリの応答の watch_services）。正しくない名前は除く。
func (c *Collector) SetWatchServices(names []string) {
	c.watch = c.watch[:0]
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSuffix(name, ".service")
		if !ValidServiceName(name) || seen[name] || len(c.watch) >= MaxWatchServices {
			continue
		}
		seen[name] = true
		c.watch = append(c.watch, name)
	}
	c.hasWatch = len(c.watch) > 0
}

func (c *Collector) watchedServices(ctx context.Context) []WatchedService {
	args := []string{"show", "--no-pager", "--property=LoadState,ActiveState,SubState", "--"}
	for _, name := range c.watch {
		args = append(args, name+".service")
	}

	out, err := c.Run(ctx, command.Options{Timeout: 10 * time.Second, Limit: 256 << 10}, "systemctl", args...)
	blocks := ParseSystemctlShow(out)
	list := make([]WatchedService, 0, len(c.watch))
	for i, name := range c.watch {
		s := WatchedService{Name: name, LoadState: "unknown", ActiveState: "unknown"}
		// systemctl show は、指定した順に、空行で区切って出す
		if err == nil && len(blocks) == len(c.watch) {
			b := blocks[i]
			s.LoadState, s.ActiveState, s.SubState = b["LoadState"], b["ActiveState"], b["SubState"]
			s.Active = s.ActiveState == "active"
		}
		list = append(list, s)
	}

	return list
}

// ParseSystemctlShow は、systemctl show の出力（KEY=VALUE の行。ユニットごとに空行で区切る）を読む。
func ParseSystemctlShow(out []byte) []map[string]string {
	var blocks []map[string]string
	current := map[string]string{}

	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			if len(current) > 0 {
				blocks = append(blocks, current)
			}
			current = map[string]string{}
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			current[key] = value
		}
	}
	if len(current) > 0 {
		blocks = append(blocks, current)
	}

	return blocks
}
