package model

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
)

// nodeIDOverride 是显式指定的节点 ID（--node-id 参数或落库值）。
var nodeIDOverride struct {
	sync.RWMutex
	value string
}

// SetNodeIDOverride 固定本机节点 ID。传入空字符串表示取消固定。
func SetNodeIDOverride(value string) {
	nodeIDOverride.Lock()
	nodeIDOverride.value = strings.TrimSpace(value)
	nodeIDOverride.Unlock()
}

// NodeIDOverride 返回当前固定的节点 ID（未固定时为空）。
func NodeIDOverride() string {
	nodeIDOverride.RLock()
	defer nodeIDOverride.RUnlock()
	return nodeIDOverride.value
}

var (
	nodeIDOnce   sync.Once
	nodeIDCached string
)

// NodeID 返回本机节点 ID，优先级：
//  1. 显式固定值（--node-id / 落库持久化的值）
//  2. 硬件唯一标识（IOPlatformUUID / MachineGuid / machine-id）
//  3. MAC 地址集合（历史行为，最后兜底）
//
// 结果只计算一次并缓存：早期实现每次调用都重新枚举网卡，
// macOS 切 Wi-Fi 时私有地址随机化会让 ID 反复变化。
func NodeID() string {
	if fixed := NodeIDOverride(); fixed != "" {
		return fixed
	}
	nodeIDOnce.Do(func() {
		if source := hardwareSource(); source != "" {
			nodeIDCached = encodeNodeID(source)
			return
		}
		nodeIDCached = encodeNodeID(macSource())
	})
	return nodeIDCached
}

// NodeIDFromMAC 保留旧名：按 MAC 集合派生节点 ID。
func NodeIDFromMAC() string {
	return encodeNodeID(macSource())
}

func hardwareSource() string {
	parts := HardwareNodeIDParts()
	if len(parts) == 0 {
		return ""
	}
	return "hw:" + strings.Join(parts, "|")
}

func macSource() string {
	var macs []string
	if interfaces, err := net.Interfaces(); err == nil {
		for _, iface := range interfaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			mac := iface.HardwareAddr.String()
			if mac != "" && mac != "00:00:00:00:00:00" {
				macs = append(macs, mac)
			}
		}
	}
	if len(macs) > 0 {
		sort.Strings(macs)
		return "mac:" + strings.Join(macs, ",")
	}
	if hostname, _ := os.Hostname(); hostname != "" {
		return "host:" + hostname
	}
	return "npa"
}

func encodeNodeID(source string) string {
	if source == "" {
		source = "npa"
	}
	sum := sha256.Sum256([]byte(strings.ToLower(source)))
	raw, _ := hex.DecodeString(hex.EncodeToString(sum[:8]))
	alphabet := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	var id strings.Builder
	for _, value := range raw {
		id.WriteByte(alphabet[int(value)%len(alphabet)])
	}
	return id.String()
}
