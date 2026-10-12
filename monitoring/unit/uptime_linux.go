//go:build linux

package monitoring

import (
	"os"
	"strconv"
	"strings"
)

var procUptimePath = "/proc/uptime"

func procUptime() (uint64, bool) {
	data, err := os.ReadFile(procUptimePath)
	if err != nil {
		return 0, false
	}
	return parseProcUptime(string(data))
}

// parseProcUptime 解析 /proc/uptime 的第一个字段（秒，带小数）。
func parseProcUptime(s string) (uint64, bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return uint64(v), true
}
