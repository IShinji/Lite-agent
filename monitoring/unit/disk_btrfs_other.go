//go:build !linux

package monitoring

// btrfsQuotaFor 仅在 Linux 上实现；其它平台始终返回 false，沿用原有的磁盘统计。
func btrfsQuotaFor(mountpoint string) (limit, used uint64, ok bool) {
	return 0, 0, false
}
