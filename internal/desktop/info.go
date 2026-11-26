package desktop

// 本文件只保留与插件之间的请求/响应契约。
// 屏幕信息（鼠标位置、分辨率、前台窗口）的采集由插件实现。

// WindowInfo 前台窗口矩形。
type WindowInfo struct {
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
	Title string `json:"title,omitempty"`
}

// MouseInfo 鼠标位置。
type MouseInfo struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// InfoResult 屏幕基础信息。
type InfoResult struct {
	Mouse     MouseInfo    `json:"mouse"`
	Screens   []Screen     `json:"screens"`
	Foreground *WindowInfo `json:"foreground,omitempty"`
}

type InfoRequest struct {
	ScreenID string `json:"screen_id,omitempty"`
}
