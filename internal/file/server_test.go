package file

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/npa/npa/internal/model"
)

// stubProvider 记录调用并回放固定结果，用于验证插件服务把远端请求转给 Provider。
type stubProvider struct {
	calls   []string
	entries []model.FileEntry
}

func (s *stubProvider) record(name, nodeID, path string) {
	s.calls = append(s.calls, name+"|"+nodeID+"|"+path)
}

func (s *stubProvider) List(_ context.Context, nodeID, path string) ([]model.FileEntry, error) {
	s.record("list", nodeID, path)
	return s.entries, nil
}

func (s *stubProvider) Stat(_ context.Context, nodeID, path string) (model.FileEntry, error) {
	s.record("stat", nodeID, path)
	if len(s.entries) == 0 {
		return model.FileEntry{}, os.ErrNotExist
	}
	return s.entries[0], nil
}

func (s *stubProvider) Read(_ context.Context, nodeID, path string) ([]byte, error) {
	s.record("read", nodeID, path)
	return []byte("remote-content"), nil
}

func (s *stubProvider) OpenRead(_ context.Context, nodeID, path string) (io.ReadCloser, error) {
	s.record("openread", nodeID, path)
	return io.NopCloser(strings.NewReader("remote-content")), nil
}

func (s *stubProvider) Write(_ context.Context, nodeID, path string, _ []byte) error {
	s.record("write", nodeID, path)
	return nil
}

func (s *stubProvider) Upload(_ context.Context, nodeID, path string, _ io.Reader) (int64, error) {
	s.record("upload", nodeID, path)
	return 4, nil
}

func (s *stubProvider) Remove(_ context.Context, nodeID, path string) error {
	s.record("remove", nodeID, path)
	return nil
}

func (s *stubProvider) Mkdir(_ context.Context, nodeID, path string) error {
	s.record("mkdir", nodeID, path)
	return nil
}

func (s *stubProvider) Rename(_ context.Context, nodeID, oldPath, newPath string) error {
	s.record("rename:"+oldPath+"->"+newPath, nodeID, "")
	return nil
}

func (s *stubProvider) Touch(_ context.Context, nodeID, path string) error {
	s.record("touch", nodeID, path)
	return nil
}

func (s *stubProvider) TouchTime(_ context.Context, nodeID, path string, _ time.Time) error {
	s.record("touchtime", nodeID, path)
	return nil
}

func (s *stubProvider) CreateReader(_ context.Context, nodeID, path string) (io.Reader, error) {
	s.record("createreader", nodeID, path)
	return strings.NewReader("remote-content"), nil
}

func get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return resp, body
}

// 插件服务在本机分支直接走文件面（不再经过 Provider）。
func TestServerServesLocalFace(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &stubProvider{}
	server := NewServer("SELF", provider, 0)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, body := get(t, ts.URL+"/api/file/list?path="+dir)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("本机列目录状态码不对: %d (%s)", resp.StatusCode, body)
	}
	var entries []fileEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("解析列目录结果失败: %v (%s)", err, body)
	}
	if len(entries) != 1 || entries[0].Name != "a.txt" || entries[0].Size != 5 {
		t.Fatalf("列目录结果不对: %#v", entries)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("本机请求不该走 Provider: %#v", provider.calls)
	}
}

// 远端请求（node 参数非空）转给 Provider。
func TestServerForwardsRemoteRequests(t *testing.T) {
	provider := &stubProvider{entries: []model.FileEntry{{Name: "remote.txt", Path: "/tmp/remote.txt", Size: 3}}}
	server := NewServer("SELF", provider, 0)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, body := get(t, ts.URL+"/api/file/list?path=/tmp&node=OTHER")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("远端列目录状态码不对: %d (%s)", resp.StatusCode, body)
	}
	var entries []fileEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("解析列目录结果失败: %v (%s)", err, body)
	}
	if len(entries) != 1 || entries[0].Name != "remote.txt" {
		t.Fatalf("远端列目录结果不对: %#v", entries)
	}
	if len(provider.calls) != 1 || provider.calls[0] != "list|OTHER|/tmp" {
		t.Fatalf("Provider 调用记录不对: %#v", provider.calls)
	}

	// 重命名走 from/to 两个路径参数。
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/file/rename?from=/tmp/a&to=/tmp/b&node=OTHER", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("远端重命名状态码不对: %d (%s)", resp.StatusCode, body)
	}
	if len(provider.calls) != 2 || !strings.HasPrefix(provider.calls[1], "rename:/tmp/a->/tmp/b") {
		t.Fatalf("Provider 调用记录不对: %#v", provider.calls)
	}
}

// 节点参数等于本机 ID 时仍走本机文件面。
func TestServerTreatsSelfIDAsLocal(t *testing.T) {
	dir := t.TempDir()
	provider := &stubProvider{}
	server := NewServer("SELF", provider, 0)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, body := get(t, ts.URL+"/api/file/list?path="+dir+"&node=SELF")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码不对: %d (%s)", resp.StatusCode, body)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("本机请求不该走 Provider: %#v", provider.calls)
	}
}

// 显式寻址别的节点时文件面直接 404（不许代打别的节点的文件面）。
// 注意：插件服务自己会把 node=别的节点 转给远端 Provider；这里测的是文件面本身。
func TestFaceRejectsForeignNode(t *testing.T) {
	mux := http.NewServeMux()
	NewFaceWithID("SELF").Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, _ := get(t, ts.URL+"/api/file/list?path=/tmp&node=OTHER")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("寻址别的节点应 404，实际 %d", resp.StatusCode)
	}
	resp, body := get(t, ts.URL+"/api/file/list?path=/tmp&node=SELF")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("寻址本机应 200，实际 %d (%s)", resp.StatusCode, body)
	}
}

// 页面与健康检查、节点列表。
func TestServerPageHealthAndNodes(t *testing.T) {
	server := NewServer("SELF", &stubProvider{}, 0)
	server.Nodes = func() []model.Node {
		return []model.Node{{ID: "SELF", Name: "本地节点"}, {ID: "N2", Name: "节点二", Address: "192.168.1.2"}}
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, body := get(t, ts.URL+"/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "NPA 文件分发") {
		t.Fatalf("插件页面不对: %d", resp.StatusCode)
	}

	resp, body = get(t, ts.URL+"/api/health")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "\"ok\":true") {
		t.Fatalf("健康检查不对: %d (%s)", resp.StatusCode, body)
	}

	resp, body = get(t, ts.URL+"/api/nodes")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("节点列表状态码不对: %d (%s)", resp.StatusCode, body)
	}
	var nodes []nodeOption
	if err := json.Unmarshal(body, &nodes); err != nil {
		t.Fatalf("解析节点列表失败: %v (%s)", err, body)
	}
	if len(nodes) != 2 {
		t.Fatalf("节点列表长度不对: %#v", nodes)
	}
	if nodes[0].ID != "SELF" || !nodes[0].Self {
		t.Fatalf("首个节点应是本机: %#v", nodes[0])
	}
	if nodes[1].ID != "N2" {
		t.Fatalf("远端节点不对: %#v", nodes[1])
	}
}

// Start/Close：监听端口与绑定范围。
func TestServerStartAndClose(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	server := NewServer("SELF", &stubProvider{}, port)
	server.SetScope("local")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Start(ctx); err != nil {
		t.Fatalf("启动文件插件服务失败: %v", err)
	}
	if server.Port() != port {
		t.Fatalf("端口不对: %d", server.Port())
	}
	resp, body := get(t, "http://127.0.0.1:"+itoa(port)+"/api/health")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "\"ok\":true") {
		t.Fatalf("健康检查不对: %d (%s)", resp.StatusCode, body)
	}
	if err := server.Close(ctx); err != nil {
		t.Fatalf("关闭文件插件服务失败: %v", err)
	}
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
