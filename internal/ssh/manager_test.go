package sshx

import (
	"strings"
	"testing"
	"time"
)

func TestLocalShellRunsCommands(t *testing.T) {
	manager := NewManager(nil)
	shell, err := manager.openLocalShell()
	if err != nil {
		t.Fatal(err)
	}
	defer shell.Close()
	_ = shell.Stdout.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := shell.Stdin.Write([]byte("echo npa-local-ok\n")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1024)
	var output strings.Builder
	for !strings.Contains(output.String(), "npa-local-ok") {
		n, err := shell.Stdout.Read(buffer)
		if n > 0 {
			output.Write(buffer[:n])
		}
		if err != nil {
			t.Fatalf("local shell output: %v, output=%q", err, output.String())
		}
	}
}
