package service

import (
	"os"
	"path/filepath"
)

// desktopPluginDirs 返回桌面控制插件（npa-remote）的查找目录。
//
// 插件有两种跑法：
//   - plugin 模式：core 以 stdio JSON-RPC 调用它，拿到截图/键鼠/剪贴板能力，
//     供 screen.* / clipboard.* 这类 MCP 工具走插件实现而不是内嵌脚本；
//   - serve 模式：sidecar 拉起 HTTP/WS 服务，给局域网浏览器做远控页面。
//
// 两种模式用同一个二进制，所以查找目录要覆盖数据目录下的 plugins/desktop
// 与 plugins（sidecar 把 npa-remote 直接放在 plugins 下）。
func desktopPluginDirs(dataDir string) []string {
	dirs := []string{}
	if dataDir != "" {
		dirs = append(dirs,
			filepath.Join(dataDir, "plugins", "desktop"),
			filepath.Join(dataDir, "plugins"))
	}
	if executable, err := os.Executable(); err == nil {
		binDir := filepath.Dir(executable)
		dirs = append(dirs,
			filepath.Join(binDir, "plugins", "desktop"),
			filepath.Join(filepath.Dir(binDir), "plugins", "desktop"),
			filepath.Join(binDir, "plugins"),
			filepath.Join(filepath.Dir(binDir), "plugins"))
	}
	return dirs
}
