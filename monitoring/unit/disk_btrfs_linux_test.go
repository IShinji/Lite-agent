//go:build linux

package monitoring

import (
	"os"
	"path/filepath"
	"testing"
)

type fakeBtrfs struct {
	t     *testing.T
	lines []string
	rdevs map[string][2]uint64
}

func newFakeBtrfs(t *testing.T) *fakeBtrfs {
	t.Helper()
	root := t.TempDir()
	oldMI, oldSys, oldBlock := btrfsMountinfoPath, btrfsSysfsRoot, btrfsBlockClass
	oldRdev := btrfsBlockRdev
	f := &fakeBtrfs{t: t, rdevs: make(map[string][2]uint64)}
	t.Cleanup(func() {
		btrfsMountinfoPath, btrfsSysfsRoot, btrfsBlockClass = oldMI, oldSys, oldBlock
		btrfsBlockRdev = oldRdev
	})
	btrfsMountinfoPath = filepath.Join(root, "mountinfo")
	btrfsSysfsRoot = filepath.Join(root, "btrfs")
	btrfsBlockClass = filepath.Join(root, "block")
	btrfsBlockRdev = f.lookupRdev
	return f
}

func (f *fakeBtrfs) lookupRdev(source string) (uint64, uint64, bool) {
	rdev, ok := f.rdevs[source]
	if !ok {
		return 0, 0, false
	}
	return rdev[0], rdev[1], true
}

// sourceRdev 模拟对挂载源 stat 得到的块设备号，不创建真实设备节点。
func (f *fakeBtrfs) sourceRdev(source, devNum string) {
	f.t.Helper()
	major, minor, ok := parseDevNum(devNum)
	if !ok {
		f.t.Fatalf("bad device number %q", devNum)
	}
	f.rdevs[source] = [2]uint64{major, minor}
}

func (f *fakeBtrfs) mount(line string) { f.lines = append(f.lines, line) }

func (f *fakeBtrfs) flush() {
	f.t.Helper()
	content := ""
	for _, line := range f.lines {
		content += line + "\n"
	}
	if err := os.WriteFile(btrfsMountinfoPath, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeBtrfs) device(fsid, name string) {
	f.t.Helper()
	dir := filepath.Join(btrfsSysfsRoot, fsid, "devices")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeBtrfs) block(name, devNum string) {
	f.t.Helper()
	dir := filepath.Join(btrfsBlockClass, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dev"), []byte(devNum+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeBtrfs) qgroup(fsid, id string, files map[string]string) {
	f.t.Helper()
	dir := filepath.Join(btrfsSysfsRoot, fsid, "qgroups", "0_"+id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	for name, val := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(val+"\n"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fakeBtrfs) apply(mountpoint string, statTotal, statUsed uint64) (uint64, uint64, string, bool) {
	f.t.Helper()
	f.flush()
	return newBtrfsProbe().apply(mountpoint, statTotal, statUsed)
}

// 仿照非特权 LXC：btrfs 根子卷，mountinfo 的设备号和块设备节点不一致，源设备名仍是 loop0。
const exampleContainerRoot = `100 50 0:60 /containers/EXAMPLE/rootfs / rw,relatime,idmapped shared:11 master:12 - btrfs /dev/loop0 rw,discard=async,space_cache=v2,user_subvol_rm_allowed,subvolid=1234,subvol=/containers/EXAMPLE`

func TestBtrfsQuotaContainerLayout(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.mount(`101 100 0:61 / /dev rw,nosuid - tmpfs tmpfs rw,size=492k`)
	f.device("00000000-0000-4000-8000-000000000001", "loop0")
	f.block("loop0", "7:0")
	f.qgroup("00000000-0000-4000-8000-000000000001", "1234", map[string]string{
		"referenced": "70000000", "exclusive": "40000000",
		"max_referenced": "1000000000", "max_exclusive": "0", "limit_flags": "1",
	})
	f.qgroup("00000000-0000-4000-8000-000000000001", "300", map[string]string{
		"referenced": "1", "max_referenced": "9", "limit_flags": "1",
	})

	total, used, subvol, quota := f.apply("/", 160<<30, 9<<30)
	if !quota || subvol != "1234" || total != 1000000000 || used != 70000000 {
		t.Fatalf("got total=%d used=%d subvol=%s quota=%v", total, used, subvol, quota)
	}
}

func TestBtrfsQuotaIgnoresAnotherFilesystemWithTheSameSubvol(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.device("11111111-1111-4111-8111-111111111111", "loop0")
	f.device("22222222-2222-4222-8222-222222222222", "sda1")
	f.qgroup("22222222-2222-4222-8222-222222222222", "1234", map[string]string{
		"referenced": "5", "max_referenced": "100", "limit_flags": "1",
	})

	total, used, subvol, quota := f.apply("/", 1000, 500)
	if quota || total != 1000 || used != 500 || subvol != "1234" {
		t.Fatalf("wrong filesystem quota must be ignored, got total=%d used=%d subvol=%s quota=%v", total, used, subvol, quota)
	}
}

func TestBtrfsQuotaUsesTheFilesystemThatOwnsTheDevice(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.device("11111111-1111-4111-8111-111111111111", "loop0")
	f.device("22222222-2222-4222-8222-222222222222", "sda1")
	f.qgroup("11111111-1111-4111-8111-111111111111", "1234", map[string]string{
		"referenced": "70", "max_referenced": "100", "limit_flags": "1",
	})
	f.qgroup("22222222-2222-4222-8222-222222222222", "1234", map[string]string{
		"referenced": "5", "max_referenced": "999", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/", 1000, 500)
	if !quota || total != 100 || used != 70 {
		t.Fatalf("got total=%d used=%d quota=%v, want 100/70", total, used, quota)
	}
}

func TestBtrfsQuotaMatchesMajorMinorWhenSourceIsNotDevName(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(`40 1 0:60 / / rw,relatime - btrfs /dev/disk/by-uuid/EXAMPLE rw,subvolid=5,subvol=/`)
	f.sourceRdev("/dev/disk/by-uuid/EXAMPLE", "8:1")
	f.device("11111111-1111-4111-8111-111111111111", "sda1")
	f.device("22222222-2222-4222-8222-222222222222", "sdb1")
	f.device("22222222-2222-4222-8222-222222222222", "EXAMPLE")
	f.block("sda1", "8:1")
	f.block("sdb1", "8:17")
	f.qgroup("11111111-1111-4111-8111-111111111111", "5", map[string]string{
		"referenced": "11", "max_referenced": "200", "limit_flags": "1",
	})
	f.qgroup("22222222-2222-4222-8222-222222222222", "5", map[string]string{
		"referenced": "99", "max_referenced": "900", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/", 5000, 1000)
	if !quota || total != 200 || used != 11 {
		t.Fatalf("got total=%d used=%d quota=%v, want 11111111-1111-4111-8111-111111111111 200/11", total, used, quota)
	}
}

func TestBtrfsQuotaSingleFilesystemWithoutDevices(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.qgroup("33333333-3333-4333-8333-333333333333", "1234", map[string]string{
		"referenced": "4", "max_referenced": "40", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/", 1000, 100)
	if !quota || total != 40 || used != 4 {
		t.Fatalf("single filesystem without a devices directory should be used, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaVisibleDevicesMustMatch(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.device("33333333-3333-4333-8333-333333333333", "sda1")
	f.qgroup("33333333-3333-4333-8333-333333333333", "1234", map[string]string{
		"referenced": "4", "max_referenced": "40", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/", 1000, 100)
	if quota || total != 1000 || used != 100 {
		t.Fatalf("a single filesystem whose devices do not match must fall back, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaDoesNotGuessAmongSeveralFilesystems(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	if err := os.MkdirAll(filepath.Join(btrfsSysfsRoot, "11111111-1111-4111-8111-111111111111"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(btrfsSysfsRoot, "22222222-2222-4222-8222-222222222222"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.qgroup("22222222-2222-4222-8222-222222222222", "1234", map[string]string{
		"referenced": "4", "max_referenced": "40", "limit_flags": "1",
	})

	if _, _, _, quota := f.apply("/", 1000, 100); quota {
		t.Fatal("two filesystems and no device match must not pick a qgroup")
	}
}

func TestBtrfsQuotaBothFilesystemsClaimTheDevice(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.device("11111111-1111-4111-8111-111111111111", "loop0")
	f.device("22222222-2222-4222-8222-222222222222", "loop0")
	vals := map[string]string{"referenced": "4", "max_referenced": "40", "limit_flags": "1"}
	f.qgroup("11111111-1111-4111-8111-111111111111", "1234", vals)
	f.qgroup("22222222-2222-4222-8222-222222222222", "1234", vals)

	if _, _, _, quota := f.apply("/", 1000, 100); quota {
		t.Fatal("two filesystems listing the same device is ambiguous")
	}
}

func TestBtrfsQuotaReservedFlagIsNotALimit(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.device("11111111-1111-4111-8111-111111111111", "loop0")
	f.qgroup("11111111-1111-4111-8111-111111111111", "1234", map[string]string{
		"referenced": "10", "max_referenced": "100", "limit_flags": "4",
	})

	total, used, _, quota := f.apply("/", 1000, 10)
	if quota || total != 1000 || used != 10 {
		t.Fatalf("reserved flag must fall back, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaMissingUsedFallsBack(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.device("11111111-1111-4111-8111-111111111111", "loop0")
	f.qgroup("11111111-1111-4111-8111-111111111111", "1234", map[string]string{
		"max_referenced": "100", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/", 1000, 10)
	if quota || total != 1000 || used != 10 {
		t.Fatalf("quota without a used value must fall back, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaNoLimit(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.device("11111111-1111-4111-8111-111111111111", "loop0")
	f.qgroup("11111111-1111-4111-8111-111111111111", "1234", map[string]string{
		"referenced": "10", "max_referenced": "0", "limit_flags": "0",
	})

	total, used, subvol, quota := f.apply("/", 1000, 10)
	if quota || total != 1000 || used != 10 || subvol != "1234" {
		t.Fatalf("missing limit must fall back, got %d/%d subvol=%s quota=%v", total, used, subvol, quota)
	}
}

func TestBtrfsQuotaExplicitZero(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.device("11111111-1111-4111-8111-111111111111", "loop0")
	f.qgroup("11111111-1111-4111-8111-111111111111", "1234", map[string]string{
		"referenced": "15", "max_referenced": "0", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/", 1000, 500)
	if !quota || total != 0 || used != 15 {
		t.Fatalf("explicit zero quota = %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaNotBelowStatfs(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.device("11111111-1111-4111-8111-111111111111", "loop0")
	f.qgroup("11111111-1111-4111-8111-111111111111", "1234", map[string]string{
		"referenced": "70", "max_referenced": "100", "limit_flags": "1",
	})

	total, used, subvol, quota := f.apply("/", 100, 50)
	if quota || total != 100 || used != 50 || subvol != "1234" {
		t.Fatalf("quota >= statfs must keep statfs, got %d/%d subvol=%s quota=%v", total, used, subvol, quota)
	}
}

func TestBtrfsQuotaUsedAbovePoolFallsBack(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.device("11111111-1111-4111-8111-111111111111", "loop0")
	f.qgroup("11111111-1111-4111-8111-111111111111", "1234", map[string]string{
		"referenced": "5000", "max_referenced": "100", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/", 1000, 100)
	if quota || total != 1000 || used != 100 {
		t.Fatalf("qgroup used above the pool must fall back, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaExclusiveOnly(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.device("11111111-1111-4111-8111-111111111111", "loop0")
	f.qgroup("11111111-1111-4111-8111-111111111111", "1234", map[string]string{
		"referenced": "900", "exclusive": "300", "max_referenced": "0", "max_exclusive": "2000", "limit_flags": "2",
	})

	total, used, _, quota := f.apply("/", 9000, 1000)
	if !quota || total != 2000 || used != 300 {
		t.Fatalf("got %d/%d quota=%v, want exclusive 2000/300", total, used, quota)
	}
}

func TestBtrfsQuotaExclusiveRemainingCanBeTighter(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.device("11111111-1111-4111-8111-111111111111", "loop0")
	f.qgroup("11111111-1111-4111-8111-111111111111", "1234", map[string]string{
		"referenced": "30", "exclusive": "10",
		"max_referenced": "80", "max_exclusive": "20", "limit_flags": "0x3",
	})

	total, used, _, quota := f.apply("/", 1000, 100)
	if !quota || total != 20 || used != 10 {
		t.Fatalf("exclusive remaining is tighter, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaHexFlagsAndLeadingZeroSubvol(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(`40 1 0:60 / / rw,relatime - btrfs /dev/loop0 rw,subvolid=0123,subvol=/`)
	f.device("11111111-1111-4111-8111-111111111111", "loop0")
	f.qgroup("11111111-1111-4111-8111-111111111111", "123", map[string]string{
		"referenced": "5", "max_referenced": "100", "limit_flags": "0x1",
	})

	total, used, subvol, quota := f.apply("/", 1000, 10)
	if !quota || total != 100 || used != 5 || subvol != "123" {
		t.Fatalf("got %d/%d subvol=%s quota=%v", total, used, subvol, quota)
	}
}

func TestBtrfsQuotaNotBtrfsOrMissing(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(`30 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,subvolid=1234`)
	f.device("11111111-1111-4111-8111-111111111111", "sda1")
	f.qgroup("11111111-1111-4111-8111-111111111111", "1234", map[string]string{
		"referenced": "5", "max_referenced": "100", "limit_flags": "1",
	})

	if _, _, _, quota := f.apply("/", 1000, 10); quota {
		t.Fatal("ext4 must not use a btrfs quota")
	}
	if _, _, subvol, quota := f.apply("/nonexistent", 1000, 10); quota || subvol != "" {
		t.Fatalf("unknown mountpoint returned subvol=%s quota=%v", subvol, quota)
	}
}

func TestBtrfsQuotaOverlayHidesUnderlyingBtrfs(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.mount(`200 100 0:70 / / rw,relatime - overlay overlay rw,lowerdir=/a,upperdir=/b`)
	f.device("11111111-1111-4111-8111-111111111111", "loop0")
	f.qgroup("11111111-1111-4111-8111-111111111111", "1234", map[string]string{
		"referenced": "5", "max_referenced": "100", "limit_flags": "1",
	})

	total, used, subvol, quota := f.apply("/", 1000, 10)
	if quota || subvol != "" || total != 1000 || used != 10 {
		t.Fatalf("overlay at / must hide the btrfs quota, got %d/%d subvol=%s quota=%v", total, used, subvol, quota)
	}
}

func TestBtrfsQuotaMountpointWithEscapedSpace(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(`40 1 0:70 / /mnt/my\040disk rw,relatime - btrfs /dev/sdb rw,subvolid=300,subvol=/data`)
	f.device("11111111-1111-4111-8111-111111111111", "sdb")
	f.qgroup("11111111-1111-4111-8111-111111111111", "300", map[string]string{
		"referenced": "7", "max_referenced": "70", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/mnt/my disk", 1000, 10)
	if !quota || total != 70 || used != 7 {
		t.Fatalf("got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaLastMountWins(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(`10 1 0:60 / / rw - btrfs /dev/loop0 rw,subvolid=5,subvol=/`)
	f.mount(`11 10 0:61 / / rw - btrfs /dev/loop1 rw,subvolid=9,subvol=/`)
	f.device("11111111-1111-4111-8111-111111111111", "loop1")
	f.qgroup("11111111-1111-4111-8111-111111111111", "9", map[string]string{
		"referenced": "2", "max_referenced": "20", "limit_flags": "1",
	})
	f.qgroup("11111111-1111-4111-8111-111111111111", "5", map[string]string{
		"referenced": "8", "max_referenced": "80", "limit_flags": "1",
	})

	total, used, subvol, quota := f.apply("/", 1000, 10)
	if !quota || subvol != "9" || total != 20 || used != 2 {
		t.Fatalf("last mount must win, got %d/%d subvol=%s quota=%v", total, used, subvol, quota)
	}
}

func TestUnescapeMountinfo(t *testing.T) {
	cases := map[string]string{
		`/a\040b`: "/a b",
		`/plain`:  "/plain",
		`/x\134y`: `/x\y`,
		`/bad\04`: `/bad\04`,
		`/t\011u`: "/t\tu",
		`/n\012u`: "/n\nu",
	}
	for in, want := range cases {
		if got := unescapeMountinfo(in); got != want {
			t.Errorf("unescapeMountinfo(%q) = %q, want %q", in, got, want)
		}
	}
}

const (
	fsFeatures = "00000000-0000-4000-8000-000000000001"
	fsMapper   = "44444444-4444-4444-8444-444444444444"
	fsOther    = "55555555-5555-4555-8555-555555555555"
)

func TestBtrfsQuotaSinglePoolIgnoresFeaturesDirectory(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.qgroup(fsFeatures, "1234", map[string]string{
		"referenced": "4", "max_referenced": "40", "limit_flags": "1",
	})
	for _, name := range []string{"features", "debug"} {
		if err := os.MkdirAll(filepath.Join(btrfsSysfsRoot, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	total, used, _, quota := f.apply("/", 1000, 100)
	if !quota || total != 40 || used != 4 {
		t.Fatalf("features directory must not block the single-pool fallback, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaTwoPoolsPlusFeaturesDoNotGuess(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	if err := os.MkdirAll(filepath.Join(btrfsSysfsRoot, fsOther), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(btrfsSysfsRoot, "features"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.qgroup(fsFeatures, "1234", map[string]string{
		"referenced": "4", "max_referenced": "40", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/", 1000, 100)
	if quota || total != 1000 || used != 100 {
		t.Fatalf("two filesystems must not guess, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaDeviceMismatchRejectsWithFeatures(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.device(fsFeatures, "sda1")
	f.qgroup(fsFeatures, "1234", map[string]string{
		"referenced": "4", "max_referenced": "40", "limit_flags": "1",
	})
	if err := os.MkdirAll(filepath.Join(btrfsSysfsRoot, "features"), 0o755); err != nil {
		t.Fatal(err)
	}

	total, used, _, quota := f.apply("/", 1000, 100)
	if quota || total != 1000 || used != 100 {
		t.Fatalf("a mismatched device must fall back even when features is present, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaMapperAliasUsesSourceRdev(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(`40 1 0:60 / / rw,relatime - btrfs /dev/mapper/vg-root rw,subvolid=5,subvol=/`)
	f.sourceRdev("/dev/mapper/vg-root", "253:0")
	f.device(fsMapper, "dm-0")
	f.block("dm-0", "253:0")
	f.device(fsOther, "sda1")
	f.block("sda1", "8:1")
	f.qgroup(fsMapper, "5", map[string]string{
		"referenced": "11", "max_referenced": "200", "limit_flags": "1",
	})
	f.qgroup(fsOther, "5", map[string]string{
		"referenced": "99", "max_referenced": "900", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/", 5000, 1000)
	if !quota || total != 200 || used != 11 {
		t.Fatalf("mapper alias must follow st_rdev to dm-0, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaConfirmedOtherBlockDeviceRejected(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(`40 1 0:60 / / rw,relatime - btrfs /dev/mapper/vg-root rw,subvolid=5,subvol=/`)
	f.sourceRdev("/dev/mapper/vg-root", "8:1")
	f.device(fsMapper, "dm-0")
	f.block("dm-0", "253:0")
	f.qgroup(fsMapper, "5", map[string]string{
		"referenced": "11", "max_referenced": "200", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/", 5000, 1000)
	if quota || total != 5000 || used != 1000 {
		t.Fatalf("a different block device must not take this pool, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaUnverifiedAliasDoesNotGuessAmongPools(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(`40 1 0:60 / / rw,relatime - btrfs /dev/mapper/vg-root rw,subvolid=5,subvol=/`)
	f.device(fsMapper, "dm-0")
	f.block("dm-0", "253:0")
	f.device(fsOther, "sda1")
	f.block("sda1", "8:1")
	f.qgroup(fsMapper, "5", map[string]string{
		"referenced": "11", "max_referenced": "200", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/", 5000, 1000)
	if quota || total != 5000 || used != 1000 {
		t.Fatalf("unverified alias among multiple pools must fall back, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaExclusiveUsedWhenReferencedIsNotBelowPool(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.device(fsFeatures, "loop0")
	f.qgroup(fsFeatures, "1234", map[string]string{
		"referenced": "30", "exclusive": "10",
		"max_referenced": "1000", "max_exclusive": "100", "limit_flags": "3",
	})

	total, used, _, quota := f.apply("/", 1000, 500)
	if !quota || total != 100 || used != 10 {
		t.Fatalf("exclusive quota below the pool must be kept, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaReferencedRemainingCanBeTighter(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.device(fsFeatures, "loop0")
	f.qgroup(fsFeatures, "1234", map[string]string{
		"referenced": "90", "exclusive": "10",
		"max_referenced": "100", "max_exclusive": "500", "limit_flags": "3",
	})

	total, used, _, quota := f.apply("/", 10000, 100)
	if !quota || total != 100 || used != 90 {
		t.Fatalf("referenced pair must stay together, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaEqualRemainingKeepsSmallerLimit(t *testing.T) {
	chosen, ok := chooseQgroupPair([]qgroupPair{
		{limit: 100, used: 40},
		{limit: 80, used: 20},
	}, 10000)
	if !ok || chosen != (qgroupPair{limit: 80, used: 20}) {
		t.Fatalf("equal remaining must keep the smaller limit as a pair, got %+v ok=%v", chosen, ok)
	}
}

func TestBtrfsQuotaOverLimitPairStaysTogether(t *testing.T) {
	chosen, ok := chooseQgroupPair([]qgroupPair{
		{limit: 100, used: 150},
		{limit: 80, used: 10},
	}, 1000)
	if !ok || chosen != (qgroupPair{limit: 100, used: 150}) {
		t.Fatalf("an over-limit pair must stay together, got %+v ok=%v", chosen, ok)
	}
}

func TestBtrfsQuotaEqualPairsKeepReferenced(t *testing.T) {
	chosen, ok := chooseQgroupPair([]qgroupPair{
		{limit: 100, used: 40},
		{limit: 100, used: 40},
	}, 10000)
	if !ok || chosen != (qgroupPair{limit: 100, used: 40}) {
		t.Fatalf("equal pairs must keep referenced, got %+v ok=%v", chosen, ok)
	}
}

func TestBtrfsQuotaEnabledLimitReadFailureFallsBack(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(exampleContainerRoot)
	f.device(fsFeatures, "loop0")
	f.qgroup(fsFeatures, "1234", map[string]string{
		"referenced": "30", "max_referenced": "80", "limit_flags": "3",
	})

	total, used, _, quota := f.apply("/", 1000, 100)
	if quota || total != 1000 || used != 100 {
		t.Fatalf("a missing exclusive limit must not be treated as absent, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaCandidateRdevUnreadableStaysUnknown(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(`40 1 0:60 / / rw,relatime - btrfs /dev/mapper/vg-root rw,subvolid=5,subvol=/`)
	f.sourceRdev("/dev/mapper/vg-root", "253:0")
	f.device(fsMapper, "dm-0")
	f.qgroup(fsMapper, "5", map[string]string{
		"referenced": "11", "max_referenced": "200", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/", 5000, 1000)
	if !quota || total != 200 || used != 11 {
		t.Fatalf("unreadable candidate identity must stay unknown, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaUnreadableCandidatesDoNotGuessAmongPools(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(`40 1 0:60 / / rw,relatime - btrfs /dev/mapper/vg-root rw,subvolid=5,subvol=/`)
	f.sourceRdev("/dev/mapper/vg-root", "253:0")
	f.device(fsMapper, "dm-0")
	f.device(fsOther, "sda1")
	f.qgroup(fsMapper, "5", map[string]string{
		"referenced": "11", "max_referenced": "200", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/", 5000, 1000)
	if quota || total != 5000 || used != 1000 {
		t.Fatalf("unreadable candidates across pools must not guess, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaPartialCandidateRdevDoesNotConfirmMismatch(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(`40 1 0:60 / / rw,relatime - btrfs /dev/mapper/vg-root rw,subvolid=5,subvol=/`)
	f.sourceRdev("/dev/mapper/vg-root", "253:0")
	f.device(fsMapper, "dm-0")
	f.device(fsMapper, "sda1")
	f.block("sda1", "8:1")
	f.qgroup(fsMapper, "5", map[string]string{
		"referenced": "11", "max_referenced": "200", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/", 5000, 1000)
	if !quota || total != 200 || used != 11 {
		t.Fatalf("one unreadable candidate must not become a confirmed mismatch, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaSameNameDifferentRdevRejected(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(`40 1 0:60 / / rw,relatime - btrfs /dev/sda1 rw,subvolid=5,subvol=/`)
	f.sourceRdev("/dev/sda1", "8:17")
	f.device(fsMapper, "sda1")
	f.block("sda1", "8:1")
	f.qgroup(fsMapper, "5", map[string]string{
		"referenced": "11", "max_referenced": "200", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/", 5000, 1000)
	if quota || total != 5000 || used != 1000 {
		t.Fatalf("a confirmed device-number conflict must win over the name, got %d/%d quota=%v", total, used, quota)
	}
}

func TestBtrfsQuotaSameNameSameRdevAccepted(t *testing.T) {
	f := newFakeBtrfs(t)
	f.mount(`40 1 0:60 / / rw,relatime - btrfs /dev/sda1 rw,subvolid=5,subvol=/`)
	f.sourceRdev("/dev/sda1", "8:1")
	f.device(fsMapper, "sda1")
	f.block("sda1", "8:1")
	f.qgroup(fsMapper, "5", map[string]string{
		"referenced": "11", "max_referenced": "200", "limit_flags": "1",
	})

	total, used, _, quota := f.apply("/", 5000, 1000)
	if !quota || total != 200 || used != 11 {
		t.Fatalf("matching name and device number must be accepted, got %d/%d quota=%v", total, used, quota)
	}
}
