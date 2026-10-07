package monitoring

import (
	"fmt"
	"strings"

	"github.com/nuomiiiii/lite-agent/runtimeconfig"
	"github.com/shirou/gopsutil/v4/disk"
)

type DiskInfo struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

// countedDisk 是一个挂载点计入某块物理设备后的容量。
// subvol 仅在该挂载点是 btrfs 子卷时有值；quota 表示 total/used 已换成该子卷的 qgroup。
type countedDisk struct {
	total  uint64
	used   uint64
	subvol string
	quota  bool
}

// subvolUsage 是同一子卷多次挂载归并后的结果。seq 是该结果第一次形成时的到达顺序。
type subvolUsage struct {
	total uint64
	used  uint64
	quota bool
	seq   int
}

// diskDevice 先按子卷归并，再决定这块设备对外的一个总量。
// 子卷号只在所属文件系统内唯一；调用方按设备分桶，不能把不同文件系统放进同一个 diskDevice。
type diskDevice struct {
	subvols map[string]subvolUsage
	seq     int
}

func newDiskDevice() *diskDevice {
	return &diskDevice{subvols: make(map[string]subvolUsage)}
}

func (d *diskDevice) add(sample countedDisk) {
	d.seq++
	incoming := subvolUsage{total: sample.total, used: sample.used, quota: sample.quota, seq: d.seq}
	current, ok := d.subvols[sample.subvol]
	if !ok {
		d.subvols[sample.subvol] = incoming
		return
	}
	d.subvols[sample.subvol] = mergeSubvolUsage(current, incoming)
}

// mergeSubvolUsage 归并同一子卷的重复挂载。
// 有效配额不会被同子卷的 statfs 样本盖掉。两边都是配额时保留更小的上限。
// 两边都是普通样本时保留更大的 Total。Total 相等时保留先到的那一组，Total 和 Used 不拆开搭配。
func mergeSubvolUsage(current, next subvolUsage) subvolUsage {
	switch {
	case current.quota && !next.quota:
		return current
	case next.quota && !current.quota:
		return next
	case current.quota && next.quota:
		if next.total < current.total {
			return next
		}
		return current
	default:
		if next.total > current.total {
			return next
		}
		return current
	}
}

// totals 在子卷归并之后选择设备总量。
// 只要还有一个子卷没有配额，就保留这些不限额子卷里更大的整池容量，避免某个子卷配额把整盘报小。
// 全部子卷都有配额时保留较大的那一个，不把 referenced 相加。
// Total 相等时保留先到的样本，因此 Used 和 Total 来自同一次统计。
func (d *diskDevice) totals() (total, used uint64) {
	unlimited := false
	for _, sample := range d.subvols {
		if !sample.quota {
			unlimited = true
			break
		}
	}
	var best subvolUsage
	found := false
	for _, sample := range d.subvols {
		if unlimited && sample.quota {
			continue
		}
		if !found || sample.total > best.total || (sample.total == best.total && sample.seq < best.seq) {
			best = sample
			found = true
		}
	}
	return best.total, best.used
}

func Disk() DiskInfo {
	diskinfo := DiskInfo{}
	// 获取所有分区，使用 true 避免物理磁盘被 gopsutil 错误排除
	usage, err := disk.Partitions(true)
	if err != nil {
		return diskinfo
	}
	// 一次采样只读一遍 mountinfo，同一轮挂载点共用这份结果。
	probe := newBtrfsProbe()
	includeMountpoints := runtimeconfig.IncludeMountpoints()
	if includeMountpoints != "" {
		// 自定义挂载点按用户列出的项相加。这里只替换每一项自己的 statfs，不去重。
		for _, mountpoint := range strings.Split(includeMountpoints, ";") {
			mountpoint = strings.TrimSpace(mountpoint)
			if mountpoint == "" {
				continue
			}
			u, err := disk.Usage(mountpoint)
			if err != nil {
				continue
			}
			total, used, _, _ := probe.apply(mountpoint, u.Total, u.Used)
			diskinfo.Total += total
			diskinfo.Used += used
		}
		return diskinfo
	}

	deviceMap := make(map[string]*diskDevice)
	for _, part := range usage {
		if !isPhysicalDisk(part) {
			continue
		}
		u, err := disk.Usage(part.Mountpoint)
		if err != nil {
			continue
		}
		total, used, subvol, quota := probe.apply(part.Mountpoint, u.Total, u.Used)
		sample := countedDisk{total: total, used: used, subvol: subvol, quota: quota}

		deviceID := part.Device
		// ZFS去重: 基于 pool 名称 (例如 pool/dataset -> pool)
		if strings.ToLower(part.Fstype) == "zfs" {
			if idx := strings.Index(deviceID, "/"); idx != -1 {
				deviceID = deviceID[:idx]
			}
		}
		dev := deviceMap[deviceID]
		if dev == nil {
			dev = newDiskDevice()
			deviceMap[deviceID] = dev
		}
		dev.add(sample)
	}
	for _, dev := range deviceMap {
		total, used := dev.totals()
		diskinfo.Total += total
		diskinfo.Used += used
	}
	return diskinfo
}

// isPhysicalDisk 判断分区是否为物理磁盘
func isPhysicalDisk(part disk.PartitionStat) bool {
	// 对于LXC等基于loop的根文件系统，始终包含根挂载点
	if part.Mountpoint == "/" {
		return true
	}
	mountpoint := strings.ToLower(part.Mountpoint)
	// 排除挂载点
	var mountpointsToExcludePerfix = []string{
		"/tmp",
		"/var/tmp",
		"/dev",
		"/run",
		"/var/lib/containers",
		"/var/lib/docker",
		"/proc",
		"/sys",
		"/sys/fs/cgroup",
		"/etc/resolv.conf",
		"/etc/host", // /etc/hosts,/etc/hostname
		"/nix/store",
	}
	for _, mp := range mountpointsToExcludePerfix {
		if mountpoint == mp || strings.HasPrefix(mountpoint, mp) {
			return false
		}
	}

	fstype := strings.ToLower(part.Fstype)

	// 针对 Linux autofs：排除自动挂载的 trigger，真实文件系统会作为单独分区出现不会被排除。
	// 将 autofs 视为“非物理磁盘”可以避免重复统计容量。
	if fstype == "autofs" && !strings.HasPrefix(part.Device, "/dev/") {
		return false
	}

	// 针对 Linux 下通过 ntfs-3g 挂载的 NTFS 分区 (fuseblk)，这是实际物理磁盘，不应排除
	if fstype == "fuseblk" {
		return true
	}
	var fstypeToExclude = []string{
		"tmpfs",
		"devtmpfs",
		"udev",
		"nfs",
		"cifs",
		"smb",
		"vboxsf",
		"9p",
		"fuse",
		"overlay",
		"proc",
		"devpts",
		"sysfs",
		"cgroup",
		"mqueue",
		"hugetlbfs",
		"debugfs",
		"binfmt_misc",
		"securityfs",
	}
	for _, fs := range fstypeToExclude {
		if fstype == fs || strings.HasPrefix(fstype, fs) {
			return false
		}
	}
	// Windows 网络驱动器通常是映射盘符，但不容易通过fstype判断
	// 可以通过opts判断，Windows网络驱动通常有相关选项
	optsStr := strings.ToLower(strings.Join(part.Opts, ","))
	if strings.Contains(optsStr, "remote") || strings.Contains(optsStr, "network") {
		return false
	}

	// 虚拟内存
	if strings.HasPrefix(part.Device, "/dev/loop") {
		return false
	}

	return true
}

func DiskList() ([]string, error) {
	diskList := []string{}
	includeMountpoints := runtimeconfig.IncludeMountpoints()
	if includeMountpoints != "" {
		includeMounts := strings.Split(includeMountpoints, ";")
		for _, mountpoint := range includeMounts {
			mountpoint = strings.TrimSpace(mountpoint)
			if mountpoint != "" {
				diskList = append(diskList, mountpoint)
			}
		}
	} else {
		usage, err := disk.Partitions(true)
		if err != nil {
			return nil, err
		}

		// 同一物理设备只保留路径最短的根挂载点
		deviceMap := make(map[string]disk.PartitionStat)
		for _, part := range usage {
			if isPhysicalDisk(part) {
				deviceID := part.Device
				// ZFS去重: 基于 pool 名称
				if strings.ToLower(part.Fstype) == "zfs" {
					if idx := strings.Index(deviceID, "/"); idx != -1 {
						deviceID = deviceID[:idx]
					}
				}

				if existing, ok := deviceMap[deviceID]; ok {
					// 优先保留路径更短的挂载点 (e.g., /volume1 优于 /volume1/@appdata/...)
					if len(part.Mountpoint) < len(existing.Mountpoint) {
						deviceMap[deviceID] = part
					}
				} else {
					deviceMap[deviceID] = part
				}
			}
		}

		for _, part := range deviceMap {
			diskList = append(diskList, fmt.Sprintf("%s (%s)", part.Mountpoint, part.Fstype))
		}
	}
	return diskList, nil
}
