package main

import "testing"

// TestMeshScopeFollowsLANSwitch 局域网开关决定 56701 是只听本机还是对局域网开放。
func TestMeshScopeFollowsLANSwitch(t *testing.T) {
	cases := []struct {
		name     string
		settings map[string]string
		want     string
	}{
		{
			name:     "lan on",
			settings: map[string]string{"mcp_enabled": "true", "peer_enabled": "true", "lan_enabled": "true"},
			want:     "lan",
		},
		{
			name:     "lan off",
			settings: map[string]string{"mcp_enabled": "true", "peer_enabled": "true", "lan_enabled": "false"},
			want:     "local",
		},
		{
			name:     "old database without the lan key",
			settings: map[string]string{"mcp_enabled": "true", "peer_enabled": "true"},
			want:     "lan",
		},
		{
			name:     "mcp and peer both off",
			settings: map[string]string{"mcp_enabled": "false", "peer_enabled": "false", "lan_enabled": "true"},
			want:     "",
		},
		{
			name:     "only peer on",
			settings: map[string]string{"mcp_enabled": "false", "peer_enabled": "true", "lan_enabled": "false"},
			want:     "local",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := meshScope(testCase.settings); got != testCase.want {
				t.Fatalf("meshScope = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestMeshAddressBindsLoopbackWhenLocal 局域网关闭时 56701 只监听回环。
func TestMeshAddressBindsLoopbackWhenLocal(t *testing.T) {
	if got := meshAddress("lan"); got != "0.0.0.0:56701" {
		t.Fatalf("lan scope must listen on all interfaces, got %q", got)
	}
	if got := meshAddress("local"); got != "127.0.0.1:56701" {
		t.Fatalf("local scope must listen on loopback only, got %q", got)
	}
	if got := meshAddress(""); got != "127.0.0.1:56701" {
		t.Fatalf("empty scope must listen on loopback only, got %q", got)
	}
}
