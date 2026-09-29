package desktop

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

//go:embed fallback/*
var fallback embed.FS

type Screen struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Width  int     `json:"width"`
	Height int     `json:"height"`
	X      int     `json:"x"`
	Y      int     `json:"y"`
	Scale  float64 `json:"scale"`
}

type Resource struct {
	Type     string `json:"type"`
	Path     string `json:"path"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

type CaptureRequest struct {
	ScreenID string `json:"screen_id"`
}

type CaptureResult struct {
	Resource Resource `json:"resource"`
}

type MoveRequest struct {
	X        int  `json:"x"`
	Y        int  `json:"y"`
	Relative bool `json:"relative"`
}

type ButtonRequest struct {
	Button string `json:"button"`
	Action string `json:"action"`
}

type ScrollRequest struct {
	DeltaX int `json:"delta_x"`
	DeltaY int `json:"delta_y"`
}

type Provider interface {
	Screens(context.Context) ([]Screen, error)
	Screenshot(context.Context, CaptureRequest) (CaptureResult, error)
	MoveMouse(context.Context, MoveRequest) error
	MouseButton(context.Context, ButtonRequest) error
	Scroll(context.Context, ScrollRequest) error
}

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type SystemProvider struct {
	runner Runner
	root   string
}

func NewSystemProvider() *SystemProvider { return &SystemProvider{runner: execRunner{}} }

func NewSystemProviderWithRoot(root string) *SystemProvider {
	return &SystemProvider{runner: execRunner{}, root: root}
}

func NewSystemProviderWithRunner(runner Runner) *SystemProvider {
	return &SystemProvider{runner: runner}
}

func (p *SystemProvider) runEmbedded(ctx context.Context, name string, args ...string) ([]byte, error) {
	data, err := fs.ReadFile(fallback, "fallback/"+name)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "windows" {
		commandArgs := append([]string{"-NoProfile", "-Command", string(data), "--", "npa"}, args...)
		return p.runner.Run(ctx, "powershell", commandArgs...)
	}
	commandArgs := append([]string{"-c", string(data), "npa"}, args...)
	return p.runner.Run(ctx, "/bin/sh", commandArgs...)
}

func defaultScreen() []Screen {
	return []Screen{{ID: "primary", Name: "Primary display", Width: 1920, Height: 1080, Scale: 1}}
}

func (p *SystemProvider) Screens(ctx context.Context) ([]Screen, error) {
	if runtime.GOOS == "darwin" {
		if output, err := p.runner.Run(ctx, "system_profiler", "SPDisplaysDataType", "-json"); err == nil {
			if screens := parseDarwinScreens(output); len(screens) > 0 {
				return screens, nil
			}
		}
	}
	if runtime.GOOS == "linux" {
		if output, err := p.runner.Run(ctx, "xrandr", "--listmonitors"); err == nil {
			if screens := parseXrandr(output); len(screens) > 0 {
				return screens, nil
			}
		}
	}
	return defaultScreen(), nil
}

func parseDarwinScreens(data []byte) []Screen {
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return nil
	}
	var screens []Screen
	var walk func(value any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			resolution, _ := typed["spdisplays_resolution"].(string)
			if resolution != "" {
				width, height := parseResolution(resolution)
				if width > 0 && height > 0 {
					name, _ := typed["_name"].(string)
					id, _ := typed["spdisplays_displayID"].(string)
					if id == "" {
						id = strconv.Itoa(len(screens) + 1)
					}
					scale := 1.0
					if scaling, ok := typed["spdisplays_ui_scaling"].(string); ok {
						if parsed, err := strconv.ParseFloat(strings.TrimSuffix(scaling, "%"), 64); err == nil && parsed > 0 {
							scale = parsed / 100
						}
					}
					screens = append(screens, Screen{ID: id, Name: name, Width: width, Height: height, Scale: scale})
				}
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(payload)
	return screens
}

var xrandrLine = regexp.MustCompile(`Monitor\s+\d+:\s+([^\s]+)\s+(\d+)/\d+x(\d+)/\d+\+(\d+)\+(\d+)`)

func parseXrandr(data []byte) []Screen {
	var screens []Screen
	for _, line := range strings.Split(string(data), "\n") {
		match := xrandrLine.FindStringSubmatch(line)
		if len(match) != 6 {
			continue
		}
		width, _ := strconv.Atoi(match[2])
		height, _ := strconv.Atoi(match[3])
		x, _ := strconv.Atoi(match[4])
		y, _ := strconv.Atoi(match[5])
		screens = append(screens, Screen{ID: match[1], Name: match[1], Width: width, Height: height, X: x, Y: y, Scale: 1})
	}
	return screens
}

func parseResolution(value string) (int, int) {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == 'x' || r == 'X' })
	if len(parts) < 2 {
		return 0, 0
	}
	width, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0
	}
	height, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0
	}
	return width, height
}

func (p *SystemProvider) Screenshot(ctx context.Context, request CaptureRequest) (CaptureResult, error) {
	if p.root != "" {
		if err := os.MkdirAll(p.root, 0o700); err != nil {
			return CaptureResult{}, err
		}
	}
	file, err := os.CreateTemp(p.root, "npa-screenshot-*.png")
	if err != nil {
		return CaptureResult{}, err
	}
	path := file.Name()
	_ = file.Close()
	var runErr error
	switch runtime.GOOS {
	case "darwin":
		args := []string{request.ScreenID, path}
		_, runErr = p.runEmbedded(ctx, "screenshot_darwin.sh", args...)
	case "windows":
		_, runErr = p.runEmbedded(ctx, "screenshot_windows.ps1", path)
	default:
		_, runErr = p.runEmbedded(ctx, "screenshot_linux.sh", path)
	}
	if runErr != nil {
		_ = os.Remove(path)
		return CaptureResult{}, fmt.Errorf("screenshot fallback failed: %w", runErr)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		_ = os.Remove(path)
		return CaptureResult{}, fmt.Errorf("screenshot produced no image")
	}
	return CaptureResult{Resource: Resource{Type: "file", Path: path, MimeType: "image/png", Size: info.Size()}}, nil
}

func (p *SystemProvider) MoveMouse(ctx context.Context, request MoveRequest) error {
	if request.X < -100000 || request.X > 100000 || request.Y < -100000 || request.Y > 100000 {
		return fmt.Errorf("mouse coordinates out of bounds")
	}
	if runtime.GOOS == "windows" {
		_, err := p.runEmbedded(ctx, "mouse_windows.ps1", "move", strconv.Itoa(request.X), strconv.Itoa(request.Y), strconv.FormatBool(request.Relative))
		return err
	}
	_, err := p.runEmbedded(ctx, scriptName("mouse"), "move", strconv.Itoa(request.X), strconv.Itoa(request.Y), strconv.FormatBool(request.Relative))
	return err
}

func (p *SystemProvider) MouseButton(ctx context.Context, request ButtonRequest) error {
	action := strings.ToLower(request.Action)
	if action != "down" && action != "up" && action != "click" {
		return fmt.Errorf("invalid mouse action")
	}
	button := strings.ToLower(request.Button)
	if button != "left" && button != "right" && button != "middle" {
		return fmt.Errorf("invalid mouse button")
	}
	if runtime.GOOS == "windows" {
		_, err := p.runEmbedded(ctx, "mouse_windows.ps1", "button", button, action)
		return err
	}
	_, err := p.runEmbedded(ctx, scriptName("mouse"), "button", button, action)
	return err
}

func (p *SystemProvider) Scroll(ctx context.Context, request ScrollRequest) error {
	if request.DeltaX < -10000 || request.DeltaX > 10000 || request.DeltaY < -10000 || request.DeltaY > 10000 {
		return fmt.Errorf("scroll delta out of bounds")
	}
	if runtime.GOOS == "windows" {
		_, err := p.runEmbedded(ctx, "mouse_windows.ps1", "scroll", strconv.Itoa(request.DeltaX), strconv.Itoa(request.DeltaY))
		return err
	}
	_, err := p.runEmbedded(ctx, scriptName("mouse"), "scroll", strconv.Itoa(request.DeltaX), strconv.Itoa(request.DeltaY))
	return err
}

func scriptName(kind string) string {
	if runtime.GOOS == "darwin" {
		return kind + "_darwin.sh"
	}
	return kind + "_linux.sh"
}

func TempResourceDirectory() (string, error) {
	return os.MkdirTemp("", "npa-resources-*")
}

func SafeResourcePath(root, path string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if absPath != absRoot && !strings.HasPrefix(absPath, absRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("resource path escapes root")
	}
	return absPath, nil
}
