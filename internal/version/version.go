// Package version 提供版本字符串比较，用于判断某个节点上的 NPA 是否比本机新。
// 支持 "0.3.1"、"v0.3.1"、"0.3.1-phase3" 这类形式；解析失败退回字典序，绝不 panic。
package version

import (
	"strconv"
	"strings"
)

type parsed struct {
	nums   [3]int
	suffix string
	ok     bool
}

func parse(value string) parsed {
	cleaned := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "v"))
	if cleaned == "" {
		return parsed{}
	}
	core, suffix := cleaned, ""
	if index := strings.IndexAny(core, "-+"); index >= 0 {
		core, suffix = core[:index], core[index+1:]
	}
	parts := strings.Split(core, ".")
	if len(parts) > len(parsed{}.nums) {
		parts = parts[:len(parsed{}.nums)]
	}
	var result parsed
	for i, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil {
			return parsed{}
		}
		result.nums[i] = number
	}
	result.suffix = suffix
	result.ok = true
	return result
}

// Compare 返回 -1 / 0 / 1：a 小于 / 等于 / 大于 b。
// 数字段按值比较；数字相同时无后缀（正式版）高于有后缀（预发布）。
func Compare(a, b string) int {
	left, right := parse(a), parse(b)
	if left.ok && right.ok {
		for i := range left.nums {
			if left.nums[i] != right.nums[i] {
				if left.nums[i] > right.nums[i] {
					return 1
				}
				return -1
			}
		}
		switch {
		case left.suffix == right.suffix:
			return 0
		case left.suffix == "":
			return 1
		case right.suffix == "":
			return -1
		default:
			return strings.Compare(left.suffix, right.suffix)
		}
	}
	// 解析不了就按字典序兜底，空串视为最旧
	return strings.Compare(strings.TrimSpace(a), strings.TrimSpace(b))
}

// IsNewer 判断 candidate 是否比 current 新（严格大于）。
func IsNewer(candidate, current string) bool { return Compare(candidate, current) > 0 }

// IsOlder 判断 candidate 是否比 current 旧。
func IsOlder(candidate, current string) bool { return Compare(candidate, current) < 0 }

// Normalized 去掉前缀 v 与首尾空白，便于展示与比较。
func Normalized(value string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "v"))
}
