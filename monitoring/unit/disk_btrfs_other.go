//go:build !linux

package monitoring

// btrfs 配额只在 Linux 上读取。其它平台保持原来的 statfs 结果。
type btrfsProbe struct{}

func newBtrfsProbe() *btrfsProbe { return &btrfsProbe{} }

func (p *btrfsProbe) apply(mountpoint string, statTotal, statUsed uint64) (total, used uint64, subvol string, quota bool) {
	return statTotal, statUsed, "", false
}
