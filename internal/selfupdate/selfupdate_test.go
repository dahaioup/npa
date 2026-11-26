package selfupdate

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIsCandidateArtifact(t *testing.T) {
	cases := map[string]bool{
		"npa-all_20261019_2016.zip":        true,
		"npa-all_20261019_2016.zip.sha256": false,
		"npa-darwin-amd64":                 true,
		"npa-windows-amd64.exe":            runtime.GOOS == "windows",
		"使用说明.txt":                         false,
		"run.bat":                          false,
		"npa-darwin-amd64.old":             false,
	}
	for name, want := range cases {
		if got := IsCandidateArtifact(name); got != want {
			t.Fatalf("IsCandidateArtifact(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestVerifyArtifactRejectsTruncatedAndWrongMagic(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte{0, 0, 0, 0}, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifact(empty); err == nil || !strings.Contains(err.Error(), "体积") {
		t.Fatalf("体积过小应被拒绝: %v", err)
	}

	big := make([]byte, minBinarySize+1024)
	wrongMagic := filepath.Join(dir, "wrong")
	if err := os.WriteFile(wrongMagic, big, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifact(wrongMagic); err == nil {
		t.Fatal("错误魔数应被拒绝")
	}

	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, append(platformMagic(), big...), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifact(good); err != nil {
		t.Fatalf("本平台魔数应通过: %v", err)
	}
}

// platformMagic 返回本平台可执行文件的头部字节。
func platformMagic() []byte {
	switch runtime.GOOS {
	case "windows":
		return []byte{'M', 'Z', 0x90, 0x00}
	case "linux":
		return []byte{0x7f, 'E', 'L', 'F'}
	default: // darwin
		return []byte{0xcf, 0xfa, 0xed, 0xfe}
	}
}

func TestExtractPicksPlatformBinary(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "npa-all_test.zip")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entries := []string{
		"npa/bin/npa-linux-arm64",
		"npa/bin/npa-windows-amd64.exe",
		"npa/bin/" + binaryName(),
		"npa/run.bat",
	}
	for _, name := range entries {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte("payload-" + name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	output, err := Extract(archive, filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(output) != binaryName() {
		t.Fatalf("应该解出本平台二进制 %s，实际 %s", binaryName(), filepath.Base(output))
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), binaryName()) {
		t.Fatalf("解错了文件: %q", string(raw))
	}

	// 裸二进制直接原样返回
	plain := filepath.Join(dir, binaryName())
	if got, err := Extract(plain, dir); err != nil || got != plain {
		t.Fatalf("裸二进制应原样返回: %v %v", got, err)
	}

	// 包里没有本平台二进制时报错
	missing := filepath.Join(dir, "missing.zip")
	if err := writeZip(missing, map[string]string{"npa/bin/npa-plan9-arm64": "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(missing, dir); err == nil {
		t.Fatal("缺少本平台二进制时应报错")
	}
}

func writeZip(path string, entries map[string]string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(file)
	for name, content := range entries {
		entry, err := writer.Create(name)
		if err != nil {
			return err
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return file.Close()
}

func TestFileSHA256(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := FileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("abc"))
	if got != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha256 不对: %s", got)
	}
}
