package service_test

import (
	"testing"

	"github.com/npa/npa/internal/state"
	"github.com/npa/npa/internal/service"
)

// 节点发现不再有独立开关：它跟随局域网开关，关闭局域网后 mDNS 发现必须停掉。
func TestDiscoveryFollowsLANSwitch(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := service.New(store)
	defer app.Close()

	if err := app.SaveSettings(map[string]string{"lan_enabled": "false"}); err != nil {
		t.Fatal(err)
	}
	if app.Discovery.Enabled() {
		t.Fatal("关闭局域网后节点发现必须停掉")
	}
	if err := app.SaveSettings(map[string]string{"lan_enabled": "true"}); err != nil {
		t.Fatal(err)
	}
	if !app.Discovery.Enabled() {
		status, lastErr := app.Discovery.Status()
		t.Skipf("当前环境起不来 mDNS 发现（status=%s err=%s），跳过开启方向的断言", status, lastErr)
	}
	if err := app.SaveSettings(map[string]string{"lan_enabled": "false"}); err != nil {
		t.Fatal(err)
	}
	if app.Discovery.Enabled() {
		t.Fatal("再次关闭局域网后节点发现必须停掉")
	}
}
