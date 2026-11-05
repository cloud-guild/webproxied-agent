package metrics

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MaxAccessLogs は、読むアクセスログの数の上限。
const MaxAccessLogs = 20

// AccessLogDir は、読んでよいアクセスログの場所（アプリの応答で、任意のファイルを読ませない）。
var AccessLogDir = "/var/log/"

// maxReadPerCollect は、1回の集計で1つのログから読む量の上限（超えた分は飛ばして、末尾から読み直す）。
const maxReadPerCollect = 128 << 20

// maxLine は、1行の長さの上限（超えた行は数えない）。
const maxLine = 64 << 10

// AccessLogSpec は、アプリが指定したアクセスログ（応答の access_logs）。
type AccessLogSpec struct {
	Path   string `json:"path"`
	Format string `json:"format"`
}

// AccessLog は、前回の送信から後に追記された行の件数。
type AccessLog struct {
	WindowSeconds int64  `json:"window_seconds"`
	Total         uint64 `json:"total"`
	Status5xx     uint64 `json:"status_5xx"`
	// Unreadable は、読めなかったログ（権限が無い・無い・場所が許されていない・形式に対応していない）。
	Unreadable []string `json:"unreadable,omitempty"`
}

// supportedFormats は、対応するログの形式（nginx・apacheの combined。状態のコードの位置は common も同じ）。
var supportedFormats = map[string]bool{"combined": true, "common": true, "nginx": true, "apache": true, "nginx_combined": true, "apache_combined": true}

// ValidAccessLogPath は、読んでよいアクセスログか（AccessLogDir の下の、正規化された絶対パス）。
func ValidAccessLogPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && strings.HasPrefix(path, AccessLogDir) && !strings.ContainsRune(path, 0)
}

// SetAccessLogs は、読むアクセスログを決める（アプリの応答の access_logs）。新しいログは、今の末尾から読む。
func (c *Collector) SetAccessLogs(specs []AccessLogSpec) {
	if len(specs) == 0 {
		if c.logs != nil {
			c.logs.close()
		}
		c.logs = nil
		return
	}

	if c.logs == nil {
		c.logs = &accessLogs{files: map[string]*logFile{}, windowStart: c.Now()}
	}

	wanted := map[string]bool{}
	for _, spec := range specs {
		if len(wanted) >= MaxAccessLogs {
			break
		}
		wanted[spec.Path] = true
		if f, ok := c.logs.files[spec.Path]; ok {
			f.format = spec.Format
			continue
		}

		f := &logFile{path: spec.Path, format: spec.Format}
		f.open(true)
		c.logs.files[spec.Path] = f
	}

	for path, f := range c.logs.files {
		if !wanted[path] {
			f.close()
			delete(c.logs.files, path)
		}
	}
}

// accessLogs は、アクセスログの読み取りの状態と、送る前の件数。
type accessLogs struct {
	files       map[string]*logFile
	windowStart time.Time
	total, s5xx uint64
	// lastCollect は、集計した時刻（送信に成功したら、次の期間の始まりにする）
	lastCollect time.Time
}

func (a *accessLogs) collect(now time.Time) *AccessLog {
	out := &AccessLog{}
	for _, f := range a.files {
		total, s5xx, err := f.read()
		a.total += total
		a.s5xx += s5xx
		if err != nil {
			out.Unreadable = append(out.Unreadable, f.path)
		}
	}

	a.lastCollect = now
	out.WindowSeconds = int64(now.Sub(a.windowStart).Round(time.Second) / time.Second)
	out.Total, out.Status5xx = a.total, a.s5xx

	return out
}

func (a *accessLogs) commit() {
	if a.lastCollect.IsZero() {
		return
	}
	a.windowStart = a.lastCollect
	a.total, a.s5xx = 0, 0
}

func (a *accessLogs) close() {
	for _, f := range a.files {
		f.close()
	}
}

// logFile は、1つのアクセスログ。ログの入れ替え（名前の変更）に備え、開いたままにして、入れ替わったら古い方を読み切ってから新しい方へ移る。
type logFile struct {
	path    string
	format  string
	f       *os.File
	offset  int64
	partial []byte
	// skipping は、次の改行までを読み飛ばす途中か（長すぎる行・飛ばした先の途中からの行）
	skipping bool
	// opened は、一度でも開けたか（後から作られたログは、先頭から読む）
	opened bool
}

var errNotAllowed = errors.New("読めないログです")

// open は、ログを開く。atEnd なら、今の末尾から読む（それまでの行は数えない）。
func (l *logFile) open(atEnd bool) error {
	if !ValidAccessLogPath(l.path) || !supportedFormats[l.format] {
		return errNotAllowed
	}

	// シンボリックリンクの先も、許された場所の中に限る
	resolved, err := filepath.EvalSymlinks(l.path)
	if err != nil {
		return err
	}
	if !ValidAccessLogPath(resolved) {
		return errNotAllowed
	}

	f, err := os.Open(resolved)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return errNotAllowed
	}

	l.f, l.offset, l.partial, l.skipping = f, 0, nil, false
	if atEnd {
		l.offset = info.Size()
	}
	l.opened = true

	return nil
}

func (l *logFile) close() {
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

// read は、前回から後に追記された行を数える。
func (l *logFile) read() (total, s5xx uint64, err error) {
	if l.f == nil {
		// 前に開けなかったログ。後から作られたなら、先頭から読む
		if err := l.open(l.opened); err != nil {
			return 0, 0, err
		}
	}

	// 今開いているファイルの続きを読む（入れ替わっていても、古い方の残りを先に読み切る）
	t, s, err := l.drain()
	total, s5xx = total+t, s5xx+s
	if err != nil {
		l.close()
		return total, s5xx, err
	}

	current, statErr := os.Stat(l.path)
	opened, _ := l.f.Stat()
	switch {
	case statErr != nil:
		// 入れ替えの途中（新しいログがまだ無い）。古い方を開いたまま、次の集計で確かめる
	case !os.SameFile(current, opened):
		// 名前を変えて入れ替えた。新しいログを先頭から読む
		l.close()
		if err := l.open(false); err != nil {
			return total, s5xx, err
		}
		t, s, err := l.drain()
		total, s5xx = total+t, s5xx+s
		if err != nil {
			l.close()
			return total, s5xx, err
		}
	case current.Size() < l.offset:
		// 中身を空にして入れ替えた（copytruncate）。先頭から読み直す
		l.offset, l.partial, l.skipping = 0, nil, false
		t, s, err := l.drain()
		total, s5xx = total+t, s5xx+s
		if err != nil {
			l.close()
			return total, s5xx, err
		}
	}

	return total, s5xx, nil
}

// drain は、offset から末尾まで読み、終わりまで書かれた行を数える（途中までの行は、次に回す）。
func (l *logFile) drain() (total, s5xx uint64, err error) {
	info, err := l.f.Stat()
	if err != nil {
		return 0, 0, err
	}
	if info.Size() < l.offset {
		// 中身が空になった（copytruncate）
		l.offset, l.partial, l.skipping = 0, nil, false
	}
	if info.Size()-l.offset > maxReadPerCollect {
		// 読み切れないほど多いときは、飛ばして末尾の方から読む（集計の時間を限る）。飛ばした先の最初の行は途中からなので、数えない
		l.offset, l.partial, l.skipping = info.Size()-maxReadPerCollect, nil, true
	}

	r := io.NewSectionReader(l.f, l.offset, info.Size()-l.offset)
	buf := make([]byte, 256<<10)
	for {
		n, readErr := r.Read(buf)
		data := buf[:n]
		l.offset += int64(n)

		for len(data) > 0 {
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				if !l.skipping && len(l.partial)+len(data) <= maxLine {
					l.partial = append(l.partial, data...)
				} else {
					// 長すぎる行は、数えない（行の終わりまで読み飛ばす）
					l.partial, l.skipping = nil, true
				}
				break
			}

			line := data[:i]
			data = data[i+1:]
			if l.skipping {
				l.skipping = false
				continue
			}
			if len(l.partial)+len(line) > maxLine {
				l.partial = nil
				continue
			}
			if len(l.partial) > 0 {
				line = append(l.partial, line...)
				l.partial = nil
			}
			if status, ok := ParseStatus(line); ok {
				total++
				if status >= 500 && status <= 599 {
					s5xx++
				}
			}
		}

		if readErr == io.EOF {
			return total, s5xx, nil
		}
		if readErr != nil {
			return total, s5xx, readErr
		}
	}
}

// ParseStatus は、combined（common）形式の行から、状態のコードを読む。
//
//	127.0.0.1 - - [10/Oct/2000:13:55:36 -0700] "GET / HTTP/1.1" 200 2326 "-" "curl/8.0"
func ParseStatus(line []byte) (int, bool) {
	i := bytes.Index(line, []byte(`] "`))
	if i < 0 {
		return 0, false
	}

	// リクエストの行の終わりの " を探す（\" はエスケープされた " ）
	j := i + 3
	for ; j < len(line); j++ {
		if line[j] == '\\' {
			j++
			continue
		}
		if line[j] == '"' {
			break
		}
	}

	rest := line[min(j+1, len(line)):]
	if len(rest) < 4 || rest[0] != ' ' || (len(rest) > 4 && rest[4] != ' ') {
		return 0, false
	}

	status := 0
	for _, ch := range rest[1:4] {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		status = status*10 + int(ch-'0')
	}

	return status, status >= 100
}
