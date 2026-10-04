package model

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"sort"
	"strings"
)

func NodeIDFromMAC() string {
	var source string
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
		source = strings.Join(macs, ",")
	}
	if source == "" {
		source, _ = os.Hostname()
	}
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
