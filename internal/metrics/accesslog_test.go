package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseStatus(t *testing.T) {
	cases := map[string]int{
		`127.0.0.1 - - [10/Oct/2000:13:55:36 -0700] "GET / HTTP/1.1" 200 2326 "-" "curl/8.0"`:            200,
		`::1 - bob [10/Oct/2000:13:55:36 +0900] "POST /api HTTP/2.0" 502 0 "https://x/" "Mozilla \"x\""`: 502,
		`1.2.3.4 - - [10/Oct/2000:13:55:36 +0000] "GET /a\" 500 b HTTP/1.1" 404 10 "-" "-"`:              404,
		`1.2.3.4 - - [10/Oct/2000:13:55:36 +0000] "\x16\x03\x01" 400 157 "-" "-"`:                        400,
		`1.2.3.4 - - [10/Oct/2000:13:55:36 +0000] "-" 503 -`:                                             503,
	}
	for line, want := range cases {
		if got, ok := ParseStatus([]byte(line)); !ok || got != want {
			t.Errorf("%s: %d %v", line, got, ok)
		}
	}

	for _, line := range []string{"", "garbage", `x [d] "GET /" abc 1`, `x [d] "GET /" 2000 1`, `x [d] "GET / HTTP/1.1"`} {
		if _, ok := ParseStatus([]byte(line)); ok {
			t.Errorf("読めない行を数える: %s", line)
		}
	}
}

func logLine(status int) string {
	return fmt.Sprintf(`10.0.0.1 - - [05/Nov/2026:10:00:00 +0900] "GET / HTTP/1.1" %d 10 "-" "test"`+"\n", status)
}

func appendLog(t *testing.T, path string, lines ...string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(strings.Join(lines, ""))
	f.Close()
}

func setupLogs(t *testing.T) (*Collector, string, *time.Time) {
	dir := t.TempDir()
	old := AccessLogDir
	AccessLogDir = dir + "/"
	t.Cleanup(func() { AccessLogDir = old })

	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	c, _ := testCollector(t, nil)
	c.Now = func() time.Time { return now }

	return c, dir, &now
}

func TestAccessLogAppendAndRotate(t *testing.T) {
	c, dir, now := setupLogs(t)
	path := filepath.Join(dir, "access.log")

	// 指定する前の行は数えない
	appendLog(t, path, logLine(200), logLine(500))
	c.SetAccessLogs([]AccessLogSpec{{Path: path, Format: "combined"}})

	appendLog(t, path, logLine(200), logLine(502), logLine(404))
	// 途中まで書かれた行は、次に回す
	appendLog(t, path, `10.0.0.1 - - [05/Nov/2026:10:00:00 +0900] "GET / HTTP/1.1" 5`)
	*now = now.Add(time.Minute)

	got := c.logs.collect(*now)
	if got.Total != 3 || got.Status5xx != 1 || got.WindowSeconds != 60 || len(got.Unreadable) != 0 {
		t.Fatalf("unexpected: %+v", got)
	}

	// 送れなかったら、次の集計に合わせる
	appendLog(t, path, "03 10 \"-\" \"test\"\n")
	*now = now.Add(time.Minute)
	got = c.logs.collect(*now)
	if got.Total != 4 || got.Status5xx != 2 || got.WindowSeconds != 120 {
		t.Fatalf("送れなかった分を合わせない: %+v", got)
	}
	c.Commit()

	// 名前を変えて入れ替える（古い方へ書かれた残りも数える）
	appendLog(t, path, logLine(500))
	os.Rename(path, path+".1")
	appendLog(t, path+".1", logLine(503))
	appendLog(t, path, logLine(200), logLine(201))
	*now = now.Add(time.Minute)
	got = c.logs.collect(*now)
	if got.Total != 4 || got.Status5xx != 2 || got.WindowSeconds != 60 {
		t.Fatalf("入れ替えを数えない: %+v", got)
	}
	c.Commit()

	// 中身を空にして入れ替える（copytruncate）
	os.Truncate(path, 0)
	appendLog(t, path, logLine(500))
	*now = now.Add(time.Minute)
	got = c.logs.collect(*now)
	if got.Total != 1 || got.Status5xx != 1 {
		t.Fatalf("copytruncateを数えない: %+v", got)
	}
	c.Commit()

	// 新しいログがまだ無い（入れ替えの途中）
	os.Rename(path, path+".2")
	*now = now.Add(time.Minute)
	if got = c.logs.collect(*now); got.Total != 0 || len(got.Unreadable) != 0 {
		t.Fatalf("unexpected: %+v", got)
	}
	appendLog(t, path, logLine(500))
	*now = now.Add(time.Minute)
	if got = c.logs.collect(*now); got.Total != 1 {
		t.Fatalf("後から作られたログを数えない: %+v", got)
	}
}

func TestAccessLogLateCreate(t *testing.T) {
	c, dir, now := setupLogs(t)
	path := filepath.Join(dir, "later.log")

	c.SetAccessLogs([]AccessLogSpec{{Path: path, Format: "nginx"}})
	if got := c.logs.collect(*now); len(got.Unreadable) != 1 {
		t.Fatalf("無いログを、読めないと伝えない: %+v", got)
	}
	c.Commit()

	appendLog(t, path, logLine(500), logLine(200))
	if got := c.logs.collect(now.Add(time.Minute)); got.Total != 2 || got.Status5xx != 1 {
		t.Fatalf("後から作られたログを、先頭から数えない: %+v", got)
	}
}

func TestAccessLogLongLine(t *testing.T) {
	c, dir, now := setupLogs(t)
	path := filepath.Join(dir, "access.log")
	appendLog(t, path)
	c.SetAccessLogs([]AccessLogSpec{{Path: path, Format: "combined"}})

	long := `10.0.0.1 - - [05/Nov/2026:10:00:00 +0900] "GET /` + strings.Repeat("a", maxLine+10) + ` HTTP/1.1" 500 1 "-" "-"` + "\n"
	appendLog(t, path, long, logLine(200))
	if got := c.logs.collect(*now); got.Total != 1 || got.Status5xx != 0 {
		t.Fatalf("長すぎる行の扱いが違う: %+v", got)
	}
}

func TestAccessLogRestrictions(t *testing.T) {
	c, dir, now := setupLogs(t)
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte(logLine(500)), 0o644)
	link := filepath.Join(dir, "link.log")
	os.Symlink(outside, link)
	os.Mkdir(filepath.Join(dir, "dir.log"), 0o755)
	appendLog(t, filepath.Join(dir, "ok.log"))

	c.SetAccessLogs([]AccessLogSpec{
		{Path: outside, Format: "combined"},
		{Path: link, Format: "combined"},
		{Path: dir + "/../x/access.log", Format: "combined"},
		{Path: "relative.log", Format: "combined"},
		{Path: filepath.Join(dir, "dir.log"), Format: "combined"},
		{Path: filepath.Join(dir, "ok.log"), Format: "json"},
	})

	got := c.logs.collect(*now)
	if len(got.Unreadable) != 6 || got.Total != 0 {
		t.Fatalf("許されていないログを読む: %+v", got)
	}

	if !ValidAccessLogPath(filepath.Join(dir, "nginx/access.log")) || ValidAccessLogPath(dir) {
		t.Fatal("ValidAccessLogPath")
	}

	// 指定から外したら、閉じて数えない
	c.SetAccessLogs([]AccessLogSpec{})
	if c.logs != nil || c.Collect(t.Context()).AccessLog != nil {
		t.Fatal("空の指定で、数えるのをやめない")
	}
}

func TestAccessLogFIFODoesNotBlock(t *testing.T) {
	c, dir, now := setupLogs(t)
	fifo := filepath.Join(dir, "fifo.log")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skip(err)
	}

	done := make(chan *AccessLog, 1)
	go func() {
		c.SetAccessLogs([]AccessLogSpec{{Path: fifo, Format: "combined"}})
		done <- c.logs.collect(*now)
	}()

	select {
	case got := <-done:
		if len(got.Unreadable) != 1 {
			t.Fatalf("名前付きパイプを読む: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("名前付きパイプを開いて止まる")
	}
}

func TestAccessLogExistingFileFixedLater(t *testing.T) {
	c, dir, now := setupLogs(t)
	path := filepath.Join(dir, "access.log")
	appendLog(t, path, logLine(500), logLine(500))

	// 形式を間違えて指定し、後から直しても、前からあった行は数えない
	c.SetAccessLogs([]AccessLogSpec{{Path: path, Format: "json"}})
	if got := c.logs.collect(*now); len(got.Unreadable) != 1 {
		t.Fatalf("unexpected: %+v", got)
	}
	c.SetAccessLogs([]AccessLogSpec{{Path: path, Format: "combined"}})
	appendLog(t, path, logLine(200))
	if got := c.logs.collect(*now); got.Total != 0 || len(got.Unreadable) != 0 {
		t.Fatalf("前からあった行を数える: %+v", got)
	}
	appendLog(t, path, logLine(200))
	if got := c.logs.collect(*now); got.Total != 1 {
		t.Fatalf("unexpected: %+v", got)
	}

	// 開いている途中で、対応していない形式に変えたら、数えるのをやめる
	c.SetAccessLogs([]AccessLogSpec{{Path: path, Format: "json"}})
	appendLog(t, path, logLine(500))
	if got := c.logs.collect(*now); got.Total != 1 || len(got.Unreadable) != 1 {
		t.Fatalf("対応していない形式でも数える: %+v", got)
	}
}

func TestAccessLogRotatedUnreadableThenReadable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootでは、権限で読めないログを作れない")
	}
	c, dir, now := setupLogs(t)
	path := filepath.Join(dir, "access.log")
	appendLog(t, path)
	c.SetAccessLogs([]AccessLogSpec{{Path: path, Format: "combined"}})

	os.Rename(path, path+".1")
	appendLog(t, path, logLine(500), logLine(200))
	os.Chmod(path, 0o000)
	if got := c.logs.collect(*now); len(got.Unreadable) != 1 {
		t.Fatalf("unexpected: %+v", got)
	}
	os.Chmod(path, 0o644)
	if got := c.logs.collect(*now); got.Total != 2 {
		t.Fatalf("入れ替わった新しいログを、先頭から数えない: %+v", got)
	}
}

func TestAccessLogDeletedIsClosed(t *testing.T) {
	c, dir, now := setupLogs(t)
	path := filepath.Join(dir, "access.log")
	appendLog(t, path)
	c.SetAccessLogs([]AccessLogSpec{{Path: path, Format: "combined"}})

	os.Remove(path)
	for i := 0; i < maxMissing; i++ {
		c.logs.collect(*now)
	}
	if c.logs.files[path].f != nil {
		t.Fatal("消えたログを開いたままにする")
	}

	appendLog(t, path, logLine(503))
	if got := c.logs.collect(*now); got.Total != 1 || got.Status5xx != 1 {
		t.Fatalf("作り直されたログを、先頭から数えない: %+v", got)
	}
}

func TestAccessLogCopytruncateLarger(t *testing.T) {
	c, dir, now := setupLogs(t)
	path := filepath.Join(dir, "access.log")
	appendLog(t, path, logLine(200))
	c.SetAccessLogs([]AccessLogSpec{{Path: path, Format: "combined"}})
	appendLog(t, path, logLine(200))
	c.logs.collect(*now)
	c.Commit()

	// 空にした後、次の集計までに、前回の位置を超えて書かれた
	os.Truncate(path, 0)
	appendLog(t, path, strings.Replace(logLine(500), "10.0.0.1", "192.168.100.200", 1), logLine(500), logLine(500))
	if got := c.logs.collect(*now); got.Total != 3 || got.Status5xx != 3 {
		t.Fatalf("copytruncateを見逃す: %+v", got)
	}
}

func TestAccessLogSymlinkedDir(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "log")
	os.Symlink(real, link)
	old := AccessLogDir
	AccessLogDir = link + "/"
	t.Cleanup(func() { AccessLogDir = old })

	c, _ := testCollector(t, nil)
	path := filepath.Join(link, "access.log")
	appendLog(t, path)
	c.SetAccessLogs([]AccessLogSpec{{Path: path, Format: "combined"}})
	appendLog(t, path, logLine(500))
	if got := c.logs.collect(time.Now()); got.Total != 1 || len(got.Unreadable) != 0 {
		t.Fatalf("リンクになったログの場所を読めない: %+v", got)
	}
}
