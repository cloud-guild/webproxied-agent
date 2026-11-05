package metrics

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/cloud-guild/webproxied-agent/internal/command"
)

// TimeSync は、時刻の同期の状態。
type TimeSync struct {
	Synced bool `json:"synced"`
	// OffsetMS は、NTPの時刻とのずれ（ミリ秒。正なら進んでいる）。chronyc で調べられたときだけ。
	OffsetMS *float64 `json:"offset_ms"`
	// Source は、調べたコマンド（chronyc・timedatectl）。
	Source string `json:"source"`
}

var shortOptions = command.Options{Timeout: 5 * time.Second, Limit: 64 << 10}

// timeSync は、chronyc（ずれも分かる）、なければ timedatectl で、時刻の同期を調べる。
func (c *Collector) timeSync(ctx context.Context) *TimeSync {
	if _, err := c.LookPath("chronyc"); err == nil {
		// -n: 名前を引かない。一般のユーザーでも、127.0.0.1 経由で読める
		if out, err := c.Run(ctx, shortOptions, "chronyc", "-n", "tracking"); err == nil {
			if t := ParseChronyTracking(out); t != nil {
				return t
			}
		}
	}

	if _, err := c.LookPath("timedatectl"); err == nil {
		if out, err := c.Run(ctx, shortOptions, "timedatectl", "show", "--property=NTPSynchronized", "--value"); err == nil {
			switch strings.TrimSpace(string(out)) {
			case "yes":
				return &TimeSync{Synced: true, Source: "timedatectl"}
			case "no":
				return &TimeSync{Synced: false, Source: "timedatectl"}
			}
		}
		// show の無い古いsystemd
		if out, err := c.Run(ctx, shortOptions, "timedatectl", "status"); err == nil {
			if synced, ok := ParseTimedatectlStatus(out); ok {
				return &TimeSync{Synced: synced, Source: "timedatectl"}
			}
		}
	}

	return nil
}

// ParseChronyTracking は、chronyc tracking の出力を読む。
//
//	System time     : 0.000012345 seconds slow of NTP time
//	Leap status     : Normal
func ParseChronyTracking(out []byte) *TimeSync {
	t := &TimeSync{Source: "chronyc"}
	found, noSource := false, false

	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)

		switch key {
		case "Reference ID":
			// 同期の元が無い（00000000）か、自分の時計（7F7F0101、LOCAL）なら、同期していない
			if id, _, _ := strings.Cut(value, " "); id == "00000000" || id == "7F7F0101" {
				noSource = true
			}
		case "Leap status":
			found = true
			t.Synced = value != "Not synchronised"
		case "System time":
			fields := strings.Fields(value)
			if len(fields) >= 3 {
				if seconds, err := strconv.ParseFloat(fields[0], 64); err == nil {
					// slow は遅れている（負）、fast は進んでいる（正）
					if fields[2] == "slow" {
						seconds = -seconds
					}
					ms := round(seconds * 1000)
					t.OffsetMS = &ms
				}
			}
		}
	}

	if !found {
		return nil
	}
	if noSource {
		t.Synced = false
	}

	return t
}

// ParseTimedatectlStatus は、timedatectl status の出力から、同期しているかを読む。
func ParseTimedatectlStatus(out []byte) (bool, bool) {
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "System clock synchronized", "NTP synchronized":
			return strings.TrimSpace(value) == "yes", true
		}
	}

	return false, false
}
