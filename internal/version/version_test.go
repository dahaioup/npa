package version

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.4.0", "0.3.1-phase3", 1},
		{"0.3.1-phase3", "0.4.0", -1},
		{"0.3.10", "0.3.9", 1},
		{"0.3.1", "0.3.1-phase3", 1}, // 正式版高于预发布
		{"0.3.1-phase3", "0.3.1", -1},
		{"v0.3.1", "0.3.1", 0},  // 前缀 v 不影响
		{" 0.3.1 ", "0.3.1", 0}, // 空白不影响
		{"0.3.1", "0.3.1", 0},
		{"0.3.1-beta", "0.3.1-alpha", 1},
		{"1.0.0", "0.9.9", 1},
		{"", "", 0},
		{"", "0.1.0", -1},
		{"0.1.0", "", 1},
		{"garbage", "0.1.0", 1}, // 解析失败退回字典序，不 panic
		{"abc", "abd", -1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Fatalf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsNewerAndIsOlder(t *testing.T) {
	if !IsNewer("0.4.0", "0.3.1-phase3") {
		t.Fatal("0.4.0 应该比 0.3.1-phase3 新")
	}
	if IsNewer("0.3.0", "0.3.1-phase3") {
		t.Fatal("0.3.0 不应比 0.3.1-phase3 新")
	}
	if IsNewer("0.3.1-phase3", "0.3.1-phase3") {
		t.Fatal("同版本不应算新版本")
	}
	if !IsOlder("0.2.0", "0.3.1-phase3") {
		t.Fatal("0.2.0 应该比 0.3.1-phase3 旧")
	}
	if Normalized(" v0.3.1-phase3 ") != "0.3.1-phase3" {
		t.Fatalf("Normalized 处理不对: %q", Normalized(" v0.3.1-phase3 "))
	}
}
