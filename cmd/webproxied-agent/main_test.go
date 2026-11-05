package main

import (
	"testing"
	"time"
)

func TestInventorySchedule(t *testing.T) {
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	s := newInventorySchedule()

	if !s.attach(now) {
		t.Fatal("起動後の最初の送信で送らない")
	}

	// 失敗するたびに、付け直すまでの間隔を延ばす（最大24時間）
	for i, want := range []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute} {
		s.failed(now)
		if s.attach(now.Add(want-time.Second)) || !s.attach(now.Add(want)) {
			t.Fatalf("%d回目の失敗: 間隔が %s ではない", i+1, want)
		}
	}
	for i := 0; i < 20; i++ {
		s.failed(now)
	}
	if s.backoff != maxInventoryBackoff {
		t.Fatalf("上限を超えて延ばす: %s", s.backoff)
	}

	// 送れたら、求められるまで送らない。求められても、10分は空ける
	s.sent(now)
	if s.attach(now.Add(time.Hour)) {
		t.Fatal("求められていないのに送る")
	}
	s.requested()
	if s.attach(now.Add(minInventoryInterval-time.Second)) || !s.attach(now.Add(minInventoryInterval)) {
		t.Fatal("続けて求められたときの間隔が違う")
	}
	s.failed(now)
	if s.backoff != minInventoryInterval {
		t.Fatalf("送れた後の失敗で、間隔を最初からにしない: %s", s.backoff)
	}
}
