package desktop

import (
	"fmt"
	"strings"
)

// 本文件只保留与插件之间的请求/响应契约与入参校验。
// OCR、输入、组合键的具体实现都在插件里（npa-remote）。

// Region 屏幕区域（全屏坐标系像素值），截图与 OCR 共用。
type Region struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func (r *Region) String() string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("%d,%d,%d,%d", r.X, r.Y, r.W, r.H)
}

func (r *Region) validate() error {
	if r == nil {
		return nil
	}
	if r.X < 0 || r.Y < 0 || r.X > 100000 || r.Y > 100000 {
		return fmt.Errorf("region origin out of bounds")
	}
	if r.W <= 0 || r.H <= 0 || r.W > 20000 || r.H > 20000 {
		return fmt.Errorf("region size must be within 1..20000")
	}
	return nil
}

// ReadRequest 读取屏幕文字：Region 为空则全屏。
type ReadRequest struct {
	ScreenID string `json:"screen_id,omitempty"`
	Region   *Region `json:"region,omitempty"`
	Lang     string `json:"lang,omitempty"`
}

// TextLine 一行识别结果及其在截图内的像素框。
type TextLine struct {
	Text string `json:"text"`
	X    int    `json:"x"`
	Y    int    `json:"y"`
	W    int    `json:"w"`
	H    int    `json:"h"`
}

// ReadResult 无论 OCR 成败都带 Status 与截图文件位置（用户要求）。
// Status: ok | ocr_failed | ocr_unavailable
type ReadResult struct {
	Status   string     `json:"status"`
	Error    string     `json:"error,omitempty"`
	Engine   string     `json:"engine,omitempty"`
	Resource Resource   `json:"resource"`
	Text     string     `json:"text,omitempty"`
	Lines    []TextLine `json:"lines,omitempty"`
}

type TypeRequest struct {
	Text string `json:"text"`
}

const maxTypeRunes = 4000

// Validate 检查输入长度上限（插件侧还会再校验一次）。
func (r TypeRequest) Validate() error {
	if len([]rune(r.Text)) > maxTypeRunes {
		return fmt.Errorf("text is too long (max %d characters)", maxTypeRunes)
	}
	return nil
}

// ---- 组合键（hotkey）----

type HotkeyRequest struct {
	Combo string `json:"combo"`
}

var (
	// hotkeyModifiers 可叠加的修饰键；hotkeyKeys 单键（字母/数字/功能/导航）。
	hotkeyModifiers = map[string]bool{
		"ctrl": true, "control": true, "alt": true, "shift": true, "cmd": true,
		"command": true, "meta": true, "win": true, "option": true,
	}
	hotkeyKeys = map[string]bool{
		"enter": true, "return": true, "tab": true, "esc": true, "escape": true,
		"backspace": true, "delete": true, "del": true, "space": true,
		"home": true, "end": true, "pageup": true, "pagedown": true,
		"up": true, "down": true, "left": true, "right": true,
		"a": true, "b": true, "c": true, "d": true, "e": true, "f": true, "g": true,
		"h": true, "i": true, "j": true, "k": true, "l": true, "m": true, "n": true,
		"o": true, "p": true, "q": true, "r": true, "s": true, "t": true, "u": true,
		"v": true, "w": true, "x": true, "y": true, "z": true,
		"f1": true, "f2": true, "f3": true, "f4": true, "f5": true, "f6": true,
		"f7": true, "f8": true, "f9": true, "f10": true, "f11": true, "f12": true,
		"0": true, "1": true, "2": true, "3": true, "4": true, "5": true,
		"6": true, "7": true, "8": true, "9": true,
	}
	maxComboParts = 4
)

func (r *HotkeyRequest) validate() error {
	combo := strings.ToLower(strings.TrimSpace(r.Combo))
	if combo == "" {
		return fmt.Errorf("combo is required (e.g. ctrl+v)")
	}
	parts := strings.Split(combo, "+")
	if len(parts) < 1 || len(parts) > maxComboParts {
		return fmt.Errorf("combo must be a key or modifier+key, e.g. esc, enter or ctrl+v")
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return fmt.Errorf("combo has empty segment")
		}
		if !hotkeyModifiers[part] && !hotkeyKeys[part] {
			return fmt.Errorf("unknown key %q in combo", part)
		}
	}
	// 末段必须是普通键而非修饰键。
	last := strings.TrimSpace(parts[len(parts)-1])
	if hotkeyModifiers[last] {
		return fmt.Errorf("combo must end with a non-modifier key")
	}
	return nil
}
