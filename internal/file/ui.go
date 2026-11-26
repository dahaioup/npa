package file

import _ "embed"

// indexHTML 是文件插件自带的前端页面（独立于 NPA 控制面）。
//
//go:embed ui/index.html
var indexHTML []byte
