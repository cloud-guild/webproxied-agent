package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOSRelease(t *testing.T) {
	values := ParseOSRelease(strings.NewReader("NAME=\"Ubuntu\"\nVERSION_ID=\"24.04\"\nID=ubuntu\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\n"))
	if values["ID"] != "ubuntu" || values["VERSION_ID"] != "24.04" || values["PRETTY_NAME"] != "Ubuntu 24.04.1 LTS" {
		t.Fatalf("unexpected: %v", values)
	}
}

func TestCollect(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) {
		full := filepath.Join(root, path)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o644)
	}
	write("etc/os-release", "ID=almalinux\nVERSION_ID=\"9.8\"\nPRETTY_NAME=\"AlmaLinux 9.8\"\n")
	write("proc/sys/kernel/osrelease", "5.14.0\n")
	write("proc/uptime", "3600.50 100.00\n")

	Root = root + "/"
	defer func() { Root = "/" }()

	info := Collect()
	if info.OSID != "almalinux" || info.OSVersion != "9.8" || info.Kernel != "5.14.0" || info.UptimeSeconds != 3600 {
		t.Fatalf("unexpected: %+v", info)
	}
}
