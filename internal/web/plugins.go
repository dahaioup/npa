package web

import (
	"net/http"

	"github.com/npa/npa/internal/policy"
	"github.com/npa/npa/internal/service"
)

// 插件仓库（插件管理器）相关的 HTTP 面。
// 这些都是本机动作：下载插件、装到自己机器上，因此只挂在回环 Web 上，
// 不进 mesh/peer 面。

// handlePluginList 返回仓库里的插件清单与本机安装状态。
func (s *Server) handlePluginList(w http.ResponseWriter, r *http.Request) {
	result, err := s.app.PluginList(policy.WithSource(r.Context(), policy.SourceWeb))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

// handlePluginStatus 返回插件管理器进程状态、仓库地址与各插件槽位状态。
func (s *Server) handlePluginStatus(w http.ResponseWriter, r *http.Request) {
	result, err := s.app.PluginManagerStatus(policy.WithSource(r.Context(), policy.SourceWeb))
	if err != nil {
		// 插件管理器没起来也要把状态报出来，页面才能显示原因。
		result = map[string]any{
			"plugin_status": "missing",
			"plugin_error":  err.Error(),
			"registry_url":  s.app.PluginRegistryURL(),
		}
	}
	result["slots"] = s.app.DescribePluginSlots()
	writeJSON(w, 200, result)
}

// handlePluginInstall 从仓库安装一个插件及其依赖。
func (s *Server) handlePluginInstall(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	result, err := s.app.PluginInstall(policy.WithSource(r.Context(), policy.SourceWeb), request.ID, request.Version)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

// handlePluginRemove 卸载一个插件。
func (s *Server) handlePluginRemove(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	result, err := s.app.PluginRemove(policy.WithSource(r.Context(), policy.SourceWeb), request.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

// handlePluginUpdate 把插件更新到仓库里的最新版本。
func (s *Server) handlePluginUpdate(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	result, err := s.app.PluginUpdate(policy.WithSource(r.Context(), policy.SourceWeb), request.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

// handlePluginSettings 返回「插件设置」页要的全部插件：启停状态 + 插件自己
// 声明的参数与当前值。
func (s *Server) handlePluginSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"plugins": s.app.PluginViews()})
}

// handlePluginSettingsUpdate 应用插件设置变更：启停开关 + 参数值。
func (s *Server) handlePluginSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	var update service.PluginSettingsUpdate
	if err := decodeJSON(r, &update); err != nil {
		writeError(w, err)
		return
	}
	views, err := s.app.UpdatePluginSettings(policy.WithSource(r.Context(), policy.SourceWeb), update)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"plugins": views})
}
