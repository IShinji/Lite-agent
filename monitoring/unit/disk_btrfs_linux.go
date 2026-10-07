//go:build linux

package monitoring

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// btrfs 子卷的磁盘配额在 qgroup 上，statfs 看不到，返回的是整个存储池。
// 较新的内核把 qgroup 放在普通文件里，容器内不用特权也能读：
//
//	/sys/fs/btrfs/<fsid>/qgroups/0_<subvolid>/{referenced,exclusive,max_referenced,max_exclusive,limit_flags}
//
// 子卷号来自 /proc/self/mountinfo 的 subvolid=。对不上这块盘、没设上限或数值异常时，调用方继续用 statfs。

var (
	btrfsMountinfoPath = "/proc/self/mountinfo"
	btrfsSysfsRoot     = "/sys/fs/btrfs"
	btrfsBlockClass    = "/sys/class/block"
)

// qgroup limit_flags 的位，对应内核 BTRFS_QGROUP_LIMIT_*。
const (
	qgroupLimitMaxRfer = 1 << 0
	qgroupLimitMaxExcl = 1 << 1
)

const (
	btrfsDeviceNo = iota
	btrfsDeviceYes
	btrfsDeviceUnknown
)

// btrfsBlockRdev 读取挂载源的块设备号（stat st_rdev）。
// mountinfo 里的 major:minor 是 btrfs 匿名 superblock，不能拿来和块设备号比较。
// 测试可以替换这个函数，避免依赖真实的 /dev 节点。
var btrfsBlockRdev = readBlockRdev

type btrfsMount struct {
	subvolID string
	source   string
	devName  string
}

type btrfsProbe struct {
	byMount map[string]btrfsMount
}

func newBtrfsProbe() *btrfsProbe {
	p := &btrfsProbe{byMount: make(map[string]btrfsMount)}
	f, err := os.Open(btrfsMountinfoPath)
	if err != nil {
		return p
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		mountpoint, fstype, ok := parseMountinfoMount(sc.Text())
		if !ok {
			continue
		}
		key := filepath.Clean(mountpoint)
		if fstype != "btrfs" {
			// 后挂上的文件系统盖住了同路径上的旧 btrfs，不能再用底下那层的配额。
			delete(p.byMount, key)
			continue
		}
		m, ok := parseBtrfsMountLine(sc.Text())
		if !ok {
			delete(p.byMount, key)
			continue
		}
		p.byMount[key] = m
	}
	if sc.Err() != nil {
		// 读到一半就停的话，可能把已经被盖住的旧挂载当成当前挂载。整份丢弃，退回 statfs。
		p.byMount = make(map[string]btrfsMount)
	}
	return p
}

// apply 在选出的 qgroup 上限严格小于 statfs 总量、且该组用量没有超过 statfs 总量时改用配额。
// 这样报告的容量不会比原来的 statfs 更大。上限写成 0 也是有效配额。
// subvol 在这是 btrfs、但没有可用配额时仍会带回，供同一子卷去重。
func (p *btrfsProbe) apply(mountpoint string, statTotal, statUsed uint64) (total, used uint64, subvol string, quota bool) {
	if p == nil {
		return statTotal, statUsed, "", false
	}
	m, ok := p.byMount[filepath.Clean(mountpoint)]
	if !ok {
		return statTotal, statUsed, "", false
	}
	subvol = m.subvolID
	dir, ok := p.qgroupDir(m)
	if !ok {
		return statTotal, statUsed, subvol, false
	}
	pairs, ok := readQgroupPairs(dir)
	if !ok {
		return statTotal, statUsed, subvol, false
	}
	chosen, ok := chooseQgroupPair(pairs, statTotal)
	if !ok {
		return statTotal, statUsed, subvol, false
	}
	return chosen.limit, chosen.used, subvol, true
}

func parseMountinfoMount(line string) (mountpoint, fstype string, ok bool) {
	pre, post, ok := splitMountinfo(line)
	if !ok || len(post) < 1 {
		return "", "", false
	}
	return unescapeMountinfo(pre[4]), post[0], true
}

func parseBtrfsMountLine(line string) (btrfsMount, bool) {
	_, post, ok := splitMountinfo(line)
	if !ok || len(post) < 3 || post[0] != "btrfs" {
		return btrfsMount{}, false
	}
	subvol := ""
	for _, opt := range strings.Split(post[2], ",") {
		if v, ok := strings.CutPrefix(opt, "subvolid="); ok && v != "" {
			subvol = v
		}
	}
	subvol = canonicalSubvol(subvol)
	if subvol == "" {
		return btrfsMount{}, false
	}
	m := btrfsMount{
		subvolID: subvol,
		source:   unescapeMountinfo(post[1]),
	}
	m.devName = btrfsDevName(m.source)
	return m, true
}

func splitMountinfo(line string) (pre, post []string, ok bool) {
	sep := strings.Index(line, " - ")
	if sep < 0 {
		return nil, nil, false
	}
	pre = strings.Fields(line[:sep])
	post = strings.Fields(line[sep+3:])
	// mount id, parent, major:minor, root, mountpoint, options。可选字段在这之后，不挪动挂载点下标。
	if len(pre) < 6 || len(post) < 1 {
		return nil, nil, false
	}
	return pre, post, true
}

// qgroupDir 返回这个挂载点自己的文件系统上的 0_<subvolid> 目录。
// 子卷号在不同 btrfs 上会重复，所以先用设备名或 major:minor 对上 fsid。
// 对上了但没有这个 qgroup，就不再去别的文件系统里借。
// 只有一个文件系统、而且它的 devices 目录读不到时才直接用它：非特权容器里常见，sysfs 能看到池子但看不到块设备。
func (p *btrfsProbe) qgroupDir(m btrfsMount) (string, bool) {
	entries, err := os.ReadDir(btrfsSysfsRoot)
	if err != nil {
		return "", false
	}
	fsids := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		// 顶层还有内核的 features 目录，调试内核也可能有别的非文件系统目录。
		if !entry.IsDir() || !isBtrfsFSID(name) {
			continue
		}
		fsids = append(fsids, name)
	}
	if len(fsids) == 0 {
		return "", false
	}

	qname := "0_" + m.subvolID
	var matched []string
	unverified := 0
	unverifiedDir := ""
	for _, fsid := range fsids {
		qdir := filepath.Join(btrfsSysfsRoot, fsid, "qgroups", qname)
		qExists := isDir(qdir)
		switch btrfsDeviceMatch(fsid, m) {
		case btrfsDeviceYes:
			if !qExists {
				return "", false
			}
			matched = append(matched, qdir)
		case btrfsDeviceNo:
		case btrfsDeviceUnknown:
			unverified++
			if qExists {
				unverifiedDir = qdir
			}
		}
	}
	if len(matched) == 1 {
		return matched[0], true
	}
	if len(matched) > 1 {
		return "", false
	}
	if len(fsids) == 1 && unverified == 1 && unverifiedDir != "" {
		return unverifiedDir, true
	}
	return "", false
}

func btrfsDeviceMatch(fsid string, m btrfsMount) int {
	devDir := filepath.Join(btrfsSysfsRoot, fsid, "devices")
	entries, err := os.ReadDir(devDir)
	if err != nil || len(entries) == 0 {
		return btrfsDeviceUnknown
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return btrfsDeviceUnknown
	}

	var rMajor, rMinor uint64
	rdevOK := false
	if btrfsBlockRdev != nil && m.source != "" {
		rMajor, rMinor, rdevOK = btrfsBlockRdev(m.source)
	}
	// 源设备号已知时只认候选的真实设备号。名称相同不能压过已经读到的号冲突。
	// 还有候选号没读到、并且没有可靠匹配时，结果是未知，而不是明确不匹配。
	if rdevOK {
		unreadable := false
		for _, name := range names {
			major, minor, ok := readBlockDev(name)
			if !ok {
				unreadable = true
				continue
			}
			if major == rMajor && minor == rMinor {
				return btrfsDeviceYes
			}
		}
		if unreadable {
			return btrfsDeviceUnknown
		}
		return btrfsDeviceNo
	}
	// 源节点不可读时，/dev/<内核名> 仍按设备名关联。别名没有内核名，保持未知。
	if m.devName != "" {
		for _, name := range names {
			if name == m.devName {
				return btrfsDeviceYes
			}
		}
		return btrfsDeviceNo
	}
	return btrfsDeviceUnknown
}

// chooseQgroupPair 在已启用的限额里选一组替换 statfs。
// referenced 和 exclusive 同时生效。某一组上限不小于 statfs，或用量超过 statfs 总量时，这组不能替换整池，但另一组仍可使用。
// 两组都能用时，剩余空间（limit-used，已用超过上限则剩余为 0）更小的更紧；剩余相同则上限更小的更紧。
// 仍然相同则保留先读到的 referenced。返回的 Total 和 Used 必须来自同一组。
// 已启用的限额读失败时，readQgroupPairs 整体失败，不能把没读到的约束当成不存在。
func chooseQgroupPair(pairs []qgroupPair, statTotal uint64) (qgroupPair, bool) {
	var chosen qgroupPair
	found := false
	for _, pair := range pairs {
		if pair.limit >= statTotal || pair.used > statTotal {
			continue
		}
		if !found || qgroupPairTighter(pair, chosen) {
			chosen = pair
			found = true
		}
	}
	return chosen, found
}

func qgroupPairTighter(a, b qgroupPair) bool {
	ar, br := qgroupRemaining(a), qgroupRemaining(b)
	if ar != br {
		return ar < br
	}
	return a.limit < b.limit
}

func qgroupRemaining(pair qgroupPair) uint64 {
	if pair.used >= pair.limit {
		return 0
	}
	return pair.limit - pair.used
}

type qgroupPair struct {
	limit uint64
	used  uint64
}

func readQgroupPairs(dir string) ([]qgroupPair, bool) {
	flags, ok := readUintFile(filepath.Join(dir, "limit_flags"))
	if !ok {
		return nil, false
	}
	specs := []struct {
		bit       uint64
		limitName string
		usedName  string
	}{
		{qgroupLimitMaxRfer, "max_referenced", "referenced"},
		{qgroupLimitMaxExcl, "max_exclusive", "exclusive"},
	}
	pairs := make([]qgroupPair, 0, 2)
	for _, spec := range specs {
		if flags&spec.bit == 0 {
			continue
		}
		limit, limitOK := readUintFile(filepath.Join(dir, spec.limitName))
		used, usedOK := readUintFile(filepath.Join(dir, spec.usedName))
		if !limitOK || !usedOK {
			return nil, false
		}
		pairs = append(pairs, qgroupPair{limit: limit, used: used})
	}
	if len(pairs) == 0 {
		return nil, false
	}
	return pairs, true
}

func readUintFile(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 0, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func readBlockRdev(source string) (major, minor uint64, ok bool) {
	if !strings.HasPrefix(source, "/dev/") {
		return 0, 0, false
	}
	var st unix.Stat_t
	if err := unix.Stat(source, &st); err != nil {
		return 0, 0, false
	}
	if st.Mode&unix.S_IFMT != unix.S_IFBLK {
		return 0, 0, false
	}
	return uint64(unix.Major(uint64(st.Rdev))), uint64(unix.Minor(uint64(st.Rdev))), true
}

func isBtrfsFSID(name string) bool {
	parts := strings.Split(name, "-")
	if len(parts) != 5 {
		return false
	}
	widths := [5]int{8, 4, 4, 4, 12}
	for i, part := range parts {
		if len(part) != widths[i] {
			return false
		}
		for _, c := range part {
			switch {
			case c >= '0' && c <= '9':
			case c >= 'a' && c <= 'f':
			case c >= 'A' && c <= 'F':
			default:
				return false
			}
		}
	}
	return true
}

func readBlockDev(name string) (major, minor uint64, ok bool) {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return 0, 0, false
	}
	b, err := os.ReadFile(filepath.Join(btrfsBlockClass, name, "dev"))
	if err != nil {
		return 0, 0, false
	}
	return parseDevNum(strings.TrimSpace(string(b)))
}

func parseDevNum(s string) (major, minor uint64, ok bool) {
	majStr, minStr, ok := strings.Cut(s, ":")
	if !ok || majStr == "" || minStr == "" || strings.Contains(minStr, ":") {
		return 0, 0, false
	}
	major, err := strconv.ParseUint(majStr, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	minor, err = strconv.ParseUint(minStr, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

func canonicalSubvol(id string) string {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return ""
	}
	return strconv.FormatUint(n, 10)
}

// btrfsDevName 只接受 /dev/<name> 这种内核设备名。
// /dev/mapper、/dev/disk/by-uuid 的最后一段不是 sysfs 设备名，改用源设备的 st_rdev。
func btrfsDevName(source string) string {
	rest, ok := strings.CutPrefix(source, "/dev/")
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return ""
	}
	if rest == "." || rest == ".." {
		return ""
	}
	return rest
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// unescapeMountinfo 还原 mountinfo 里的八进制转义（\040 空格、\011 制表符、\012 换行、\134 反斜杠）。
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
