package desktop

import (
	"context"
	"strings"
	"testing"
)

type recordingRunner struct {
	name string
	args []string
	out  []byte
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.name, r.args = name, args
	return r.out, nil
}

func TestScreensParsesDisplayInfo(t *testing.T) {
	runner := &recordingRunner{out: []byte(`Monitor 0: eDP-1 1920/344x1080/194+0+0`)}
	provider := NewSystemProviderWithRunner(runner)
	screens, err := provider.Screens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(screens) != 1 || screens[0].Width != 1920 || screens[0].Height != 1080 {
		t.Fatalf("unexpected screens: %#v", screens)
	}
}

func TestMouseValidation(t *testing.T) {
	runner := &recordingRunner{}
	provider := NewSystemProviderWithRunner(runner)
	if err := provider.MoveMouse(context.Background(), MoveRequest{X: 100001, Y: 0}); err == nil {
		t.Fatal("expected coordinate validation error")
	}
	if err := provider.MouseButton(context.Background(), ButtonRequest{Button: "invalid", Action: "click"}); err == nil {
		t.Fatal("expected button validation error")
	}
	if err := provider.Scroll(context.Background(), ScrollRequest{DeltaY: 10001}); err == nil {
		t.Fatal("expected scroll validation error")
	}
}

func TestSafeResourcePath(t *testing.T) {
	root := t.TempDir()
	if _, err := SafeResourcePath(root, root+"/image.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeResourcePath(root, "/etc/passwd"); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected path traversal error, got %v", err)
	}
}
