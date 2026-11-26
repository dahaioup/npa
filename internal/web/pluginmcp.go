package web

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

	"github.com/npa/npa/internal/service"
)

// 插件 MCP 代理。
//
// 插件可以在自己的 HTTP 服务上提供 MCP（/mcp）。插件只需要绑定 127.0.0.1：
// 想让别的机器用它，走 NPA 的代理——NPA 在自己的两个面上各挂一条路径：
//
//	56700（本地面）：/plugins/<槽位>/mcp → http://127.0.0.1:<端口>/mcp
//	56701（mesh 面）：同一路径，但插件的 MCP 开关关掉时直接 403
//
// 也就是说：插件的 MCP 关掉后只能通过 56700 访问，不允许通过 56701 访问。

// pluginMCPPathPrefix 是插件 MCP 代理路径的前缀。
const pluginMCPPathPrefix = "/plugins/"

// PluginMCPHandler 返回某个插件 MCP 的代理处理器。
//
// face 决定这条代理在哪个面上生效：local（本地面 56700）与 mesh（对外 56701）。
// 插件的 MCP 关掉时，mesh 面直接拒绝。
func PluginMCPHandler(app *service.Service, slot string, face service.MCPFace) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := app.PluginHTTP(slot)
		if info == nil || !info.HasMCP {
			http.Error(w, "plugin does not provide an MCP service", http.StatusNotFound)
			return
		}
		if !app.PluginMCPAllowed(slot, string(face)) {
			if face == service.MCPFaceMesh {
				http.Error(w, "plugin MCP service is disabled; it stays available on the local face (56700)", http.StatusForbidden)
				return
			}
			http.Error(w, "plugin MCP service is not available", http.StatusForbidden)
			return
		}
		proxy := newPluginMCPProxy(info.Port)
		proxy.ServeHTTP(w, r)
	})
}

// newPluginMCPProxy 造一个指向插件 MCP 端点的反向代理。
//
// 插件永远只绑本机（127.0.0.1）：NPA 从回环访问它，外部设备通过 NPA 的
// 56701 用它的 MCP。
func newPluginMCPProxy(port int) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(port), Path: "/mcp"}
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = target.Path
			req.Host = target.Host
		},
		// SSE 要即时下推，代理不能攒着缓冲。
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "plugin MCP endpoint unavailable: "+err.Error(), http.StatusBadGateway)
		},
	}
	return proxy
}

// PluginMCPPath 解析 /plugins/<槽位>/mcp 形式的路径，返回槽位名与是否匹配。
func PluginMCPPath(path string) (string, bool) {
	if len(path) <= len(pluginMCPPathPrefix) {
		return "", false
	}
	rest := path[len(pluginMCPPathPrefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			if rest[i+1:] != "mcp" {
				return "", false
			}
			return rest[:i], true
		}
	}
	return "", false
}
