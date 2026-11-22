package apps

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// maxFileSize は、1つのファイルを読む大きさの上限。
const maxFileSize = 10 << 20

var (
	// errOutside は、アプリのディレクトリの外を指すシンボリックリンク（辿らない）。
	errOutside = errors.New("outside of the application directory")
	// errTooLarge は、上限より大きいファイル。
	errTooLarge = errors.New("too large")
	// errNotRegular は、普通のファイルでないもの（FIFO・デバイスなど。開くと止まるおそれがある）。
	errNotRegular = errors.New("not a regular file")
)

// appDir は、アプリのディレクトリ（実体）。中のファイルは、ディレクトリの外を指すリンクを辿らずに読む。
//
// 調べてから開くまでの間に、途中のディレクトリをリンクへ差し替えられても外へ出ないよう、開くのは os.Root を通す
// （os.Root は、パスの要素ごとに辿り、ルートの外へ出るものを拒む）。
type appDir struct {
	root string
	r    *os.Root
}

func openAppDir(real string) (appDir, error) {
	r, err := os.OpenRoot(real)
	if err != nil {
		return appDir{}, err
	}

	return appDir{root: real, r: r}, nil
}

func (d appDir) close() {
	d.r.Close()
}

// resolve は、ディレクトリの中の相対パスを、リンクを解決した実体の相対パスにする（外を指すリンクなら errOutside）。
// 中を指す絶対パスのリンクも辿れるよう、ここで解決してから os.Root で開く。
func (d appDir) resolve(rel string) (string, error) {
	real, err := filepath.EvalSymlinks(filepath.Join(d.root, rel))
	if err != nil {
		return "", err
	}
	if !within(d.root, real) {
		return "", errOutside
	}

	inner, err := filepath.Rel(d.root, real)
	if err != nil {
		return "", errOutside
	}

	return inner, nil
}

func within(root, p string) bool {
	if root == "/" {
		return strings.HasPrefix(p, "/")
	}

	return p == root || strings.HasPrefix(p, root+"/")
}

// open は、普通のファイルだけを開く（FIFOで止まらないよう O_NONBLOCK）。
func (d appDir) open(rel string) (*os.File, os.FileInfo, error) {
	inner, err := d.resolve(rel)
	if err != nil {
		return nil, nil, err
	}

	f, err := d.r.OpenFile(inner, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, errNotRegular
	}

	return f, info, nil
}

// readFile は、ファイルの全体を読む（maxFileSize まで）。
func (d appDir) readFile(rel string) ([]byte, error) {
	f, info, err := d.open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if info.Size() > maxFileSize {
		return nil, errTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileSize {
		return nil, errTooLarge
	}

	return data, nil
}

// readHead は、ファイルの先頭だけを読む。
func (d appDir) readHead(rel string, n int64) ([]byte, error) {
	f, _, err := d.open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return io.ReadAll(io.LimitReader(f, n))
}

// list は、ディレクトリの中の項目のうち keep が残すものを、limit 個まで、名前の順に返す（隠しファイルは除く）。
func (d appDir) list(rel string, limit int, keep func(name string, e fs.DirEntry) bool) ([]string, error) {
	inner, err := d.resolve(rel)
	if err != nil {
		return nil, err
	}
	f, err := d.r.Open(inner)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var names []string
	for len(names) < limit {
		entries, err := f.ReadDir(256)
		for _, e := range entries {
			if len(names) >= limit {
				break
			}
			if !strings.HasPrefix(e.Name(), ".") && keep(e.Name(), e) {
				names = append(names, e.Name())
			}
		}
		if errors.Is(err, io.EOF) || len(entries) == 0 {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(names)

	return names, nil
}

// subdirs は、ディレクトリの中のディレクトリ（中を指すリンクを含む）を返す。
func (d appDir) subdirs(rel string, limit int) ([]string, error) {
	return d.list(rel, limit, func(name string, e fs.DirEntry) bool {
		if e.Type()&fs.ModeSymlink == 0 {
			return e.IsDir()
		}
		inner, err := d.resolve(filepath.Join(rel, name))
		if err != nil {
			return false
		}
		info, err := d.r.Stat(inner)
		return err == nil && info.IsDir()
	})
}

// files は、ディレクトリの直下の、拡張子が ext のファイル（中を指すリンクを含む）を返す。
func (d appDir) files(rel, ext string, limit int) ([]string, error) {
	return d.list(rel, limit, func(name string, e fs.DirEntry) bool {
		return !e.IsDir() && strings.HasSuffix(name, ext)
	})
}

// exists は、ディレクトリの中にあるか（外を指すリンクは、無いものとする）。
func (d appDir) exists(rel string) bool {
	_, err := d.resolve(rel)
	return err == nil
}
