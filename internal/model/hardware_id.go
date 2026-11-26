package model

import (
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

// HardwareNodeIDParts 返回本机硬件层的稳定标识片段。
//
// 之所以不用 MAC：macOS 的「私有 Wi-Fi 地址」会随网络切换随机化，
// awdl0 / 雷雳桥接接口也会上下线，任何一项变化都会让「按 MAC 派生的
// 节点 ID」变一次——节点在拓扑里就会变成一个全新的陌生节点。
//
// 采集内容（全部只取「本机固有」的部分，不受外接设备影响）：
//   - 机器唯一 UUID：IOPlatformUUID / 注册表 MachineGuid / DMI product_uuid
//   - 机器序列号：IOPlatformSerialNumber / BIOS serial / DMI product_serial
//   - CPU 标识：仅在系统真的暴露 CPU 序列号时采集（Windows Win32_Processor.SerialNumber、
//     Linux /proc/cpuinfo 的 Serial）；拿不到就跳过，绝不用型号充数
//   - 系统盘序列号：只取「系统所在的那块盘」，明确排除 USB / 可移动设备
//
// 全部取不到时返回空片段，由调用方回退到 MAC 派生。
func HardwareNodeIDParts() []string {
	switch runtime.GOOS {
	case "darwin":
		return darwinHardwareIDs()
	case "windows":
		return windowsHardwareIDs()
	case "linux":
		return linuxHardwareIDs()
	default:
		return unixHardwareIDs()
	}
}

// --- macOS ---

func darwinHardwareIDs() []string {
	parts := []string{}
	if out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output(); err == nil {
		text := string(out)
		parts = append(parts, quotedValue(text, "IOPlatformUUID"))
		parts = append(parts, quotedValue(text, "IOPlatformSerialNumber"))
	}
	parts = append(parts, darwinSystemDiskSerial())
	return compactParts(parts...)
}

// darwinSystemDiskSerial 返回系统盘（启动盘）的序列号。
//
// 做法：diskutil 定位系统所在的物理盘（diskN），再从 system_profiler 的
// 块设备清单里按 BSD Name 匹配出它的 Serial Number。
// 只匹配「系统盘」本身，USB / 外接硬盘不在候选里，所以插拔 U 盘不会影响。
func darwinSystemDiskSerial() string {
	disk := darwinSystemDiskName()
	if disk == "" {
		return ""
	}
	out, err := exec.Command("system_profiler", "SPNVMeDataType").Output()
	if err != nil {
		return ""
	}
	return diskSerialByBSDName(string(out), disk)
}

// darwinSystemDiskName 找出承载当前系统的物理盘名（形如 disk0）。
// 优先用 diskutil info -plist / 的 APFSPhysicalStore / ParentWholeDisk 链，
// 退回 BooterDeviceIdentifier，再退回物理盘清单里第一个 internal 盘。
func darwinSystemDiskName() string {
	if out, err := exec.Command("diskutil", "info", "-plist", "/").Output(); err == nil {
		// "/" 通常挂在 APFS 容器里：APFSPhysicalStore 指向物理分区，
		// 再往上就是物理盘（disk0）。
		if store := plistValue(string(out), "APFSPhysicalStore"); store != "" {
			if out, err := exec.Command("diskutil", "info", "-plist", store).Output(); err == nil {
				if name := plistValue(string(out), "ParentWholeDisk"); name != "" {
					return name
				}
			}
			return store
		}
		// 非 APFS：直接取物理盘。
		if name := plistValue(string(out), "ParentWholeDisk"); name != "" {
			if out, err := exec.Command("diskutil", "info", "-plist", name).Output(); err == nil {
				if parent := plistValue(string(out), "ParentWholeDisk"); parent != "" {
					return parent
				}
			}
			return name
		}
	}
	return ""
}

// diskSerialByBSDName 在 system_profiler 的块设备文本里按 "BSD Name: diskN"
// 找到那一段里的 "Serial Number:"。
func diskSerialByBSDName(text, disk string) string {
	blocks := strings.Split(text, "\n\n")
	for _, block := range blocks {
		if !strings.Contains(block, "BSD Name: "+disk+"\n") {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "Serial Number:") {
				continue
			}
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "Serial Number:"))
			// 内置盘的序列号长这样；占位值（如 "000000000000"）不算。
			if len(value) < 8 || strings.Trim(value, "0") == "" {
				continue
			}
			return value
		}
	}
	return ""
}

// --- Windows ---

func windowsHardwareIDs() []string {
	parts := []string{}
	// MachineGuid 每台 Windows 机器唯一，重装系统也不会变。
	if out, err := exec.Command("reg", "query", `HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid").Output(); err == nil {
		fields := strings.Fields(string(out))
		if len(fields) > 0 {
			parts = append(parts, fields[len(fields)-1])
		}
	}
	// BIOS 序列号 + CPU 序列号 + 系统盘序列号，一条 PowerShell 取齐。
	script := `$ErrorActionPreference='SilentlyContinue;` +
		`$bios=(Get-CimInstance Win32_BIOS).SerialNumber;` +
		`$cpu=(Get-CimInstance Win32_Processor | Select-Object -First 1).SerialNumber;` +
		`$disk=(Get-Partition -DriveLetter ($env:SystemDrive[0]) | Get-Disk).SerialNumber;` +
		`Write-Output $bios;Write-Output $cpu;Write-Output $disk`
	if out, err := exec.Command("powershell", "-NoProfile", "-Command", script).Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			parts = append(parts, strings.TrimSpace(line))
		}
	}
	return compactParts(parts...)
}

// --- Linux ---

func linuxHardwareIDs() []string {
	parts := []string{}
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if raw, err := os.ReadFile(path); err == nil {
			if value := strings.TrimSpace(string(raw)); value != "" {
				parts = append(parts, value)
				break
			}
		}
	}
	if raw, err := os.ReadFile("/sys/class/dmi/id/product_uuid"); err == nil {
		parts = append(parts, strings.TrimSpace(string(raw)))
	}
	if raw, err := os.ReadFile("/sys/class/dmi/id/product_serial"); err == nil {
		parts = append(parts, strings.TrimSpace(string(raw)))
	}
	parts = append(parts, linuxCPUSerial())
	parts = append(parts, linuxSystemDiskSerial())
	return compactParts(parts...)
}

// linuxCPUSerial 从 /proc/cpuinfo 里取真正的 CPU 序列号。
// x86 通常没有这个字段（只有型号），这时返回空——不用型号充数，
// 免得换 CPU 就换身份。
func linuxCPUSerial() string {
	raw, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "serial", "serial number", "cpu serial":
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// linuxSystemDiskSerial 返回系统盘序列号。
// 先从 /proc/mounts 找到挂在 "/" 的块设备，再退到它的父盘
// （分区 sda1 → 物理盘 sda），最后读 /sys/block/<父盘>/device/serial。
// 只看根分区所在的盘，插拔 U 盘、移动硬盘都不影响。
func linuxSystemDiskSerial() string {
	disk := linuxRootBlockDevice()
	if disk == "" {
		return ""
	}
	for _, path := range []string{
		"/sys/block/" + disk + "/device/serial",
		"/sys/block/" + disk + "/serial",
	} {
		if raw, err := os.ReadFile(path); err == nil {
			if value := strings.TrimSpace(string(raw)); value != "" {
				return value
			}
		}
	}
	return ""
}

func linuxRootBlockDevice() string {
	raw, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != "/" {
			continue
		}
		device := fields[0]
		if !strings.HasPrefix(device, "/dev/") {
			continue
		}
		name := strings.TrimPrefix(device, "/dev/")
		// 分区设备去掉分区后缀，得到物理盘名。
		if index := strings.LastIndex(name, "p"); index > 0 && isAllDigits(name[index+1:]) {
			name = name[:index]
		} else if len(name) > 3 && isAllDigits(name[len(name)-1:]) {
			name = name[:len(name)-1]
		}
		return name
	}
	return ""
}

func isAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// --- 其它 Unix ---

func unixHardwareIDs() []string {
	var parts []string
	if raw, err := os.ReadFile("/etc/machine-id"); err == nil {
		parts = append(parts, strings.TrimSpace(string(raw)))
	}
	return compactParts(parts...)
}

// --- 通用小工具 ---

func compactParts(values ...string) []string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		parts = append(parts, value)
	}
	return parts
}

var quotedPattern = map[string]*regexp.Regexp{
	"IOPlatformUUID":         regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([^"]+)"`),
	"IOPlatformSerialNumber": regexp.MustCompile(`"IOPlatformSerialNumber"\s*=\s*"([^"]+)"`),
}

func quotedValue(text, key string) string {
	pattern, ok := quotedPattern[key]
	if !ok {
		return ""
	}
	match := pattern.FindStringSubmatch(text)
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

// plistValue 从 diskutil -plist 的输出里取某个 <key> 对应的 <string>。
func plistValue(text, key string) string {
	pattern := regexp.MustCompile(`<key>` + regexp.QuoteMeta(key) + `</key>\s*<string>([^<]*)</string>`)
	match := pattern.FindStringSubmatch(text)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}
