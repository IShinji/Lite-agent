package monitoring

import (
	"github.com/shirou/gopsutil/v4/host"
)

func Uptime() (uint64, error) {
	// LXC/LXD 容器里 sysinfo(2)（gopsutil 的 host.Uptime 用它）返回的是宿主机的开机时长，
	// 而 /proc/uptime 经 lxcfs 虚拟化后是容器自己的。读得到就优先用它。
	if up, ok := procUptime(); ok {
		return up, nil
	}
	return host.Uptime()
}
