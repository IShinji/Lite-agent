//go:build linux

package monitoring

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseProcUptime(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"1359.82 1359.82\n", 1359, true},
		{"0.50 0.10", 0, true},
		{"", 0, false},
		{"abc 1", 0, false},
		{"-1 2", 0, false},
	}
	for _, c := range cases {
		got, ok := parseProcUptime(c.in)
		if got != c.want || ok != c.ok {
			t.Fatalf("parseProcUptime(%q) = %d,%v; want %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestUptimePrefersProcUptime(t *testing.T) {
	old := procUptimePath
	defer func() { procUptimePath = old }()
	p := filepath.Join(t.TempDir(), "uptime")
	if err := os.WriteFile(p, []byte("1359.82 1359.82\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	procUptimePath = p
	got, err := Uptime()
	if err != nil || got != 1359 {
		t.Fatalf("Uptime() = %d,%v; want 1359,nil", got, err)
	}
	procUptimePath = filepath.Join(t.TempDir(), "missing")
	if got, err := Uptime(); err != nil || got == 1359 {
		t.Fatalf("缺文件时应回退 gopsutil：got %d,%v", got, err)
	}
}
