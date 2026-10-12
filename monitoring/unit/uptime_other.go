//go:build !linux

package monitoring

// /proc/uptime 只在 Linux 上存在；其它平台沿用 gopsutil 的结果。
func procUptime() (uint64, bool) { return 0, false }
