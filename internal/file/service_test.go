package file

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pkg/sftp"

	"github.com/npa/npa/internal/plugin"
)

// fakeInvoker 是插件调用面的测试替身：记录每次调用，可指定返回状态与结果。
type fakeInvoker struct {
	status string
	info   plugin.Info
	result any
	err    error
	calls  []fakeCall
}

type fakeCall struct {
	method string
	params fileParams
}

func (f *fakeInvoker) Invoke(_ context.Context, method string, params any, result any) error {
	call := fakeCall{method: method}
	if p, ok := params.(fileParams); ok {
		call.params = p
	}
	f.calls = append(f.calls, call)
	if f.err != nil {
		return f.err
	}
	if f.result == nil || result == nil {
		return nil
	}
	// 真实链路是 JSON 编解码往返，替身照做一遍，避免测试只在 Go 结构体上成立。
	data, err := json.Marshal(f.result)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, result)
}

func (f *fakeInvoker) Status() (string, string) { return f.status, "" }

func (f *fakeInvoker) Info() plugin.Info { return f.info }

// newBuiltinHit 造一个「命中内置实现」的哨兵：内置实现走反向链路分支，
// 由 PeerFS 直接返回哨兵错误，测试据此断言调用确实落到了内置实现。
func newBuiltinHit(t *testing.T) (*Builtin, error) {
	t.Helper()
	builtin := NewBuiltin(nil)
	builtin.IsPeer = func(string) bool { return true }
	sentinel := errors.New("builtin-hit")
	builtin.PeerFS = func(context.Context, string) (*sftp.Client, func(), error) {
		return nil, func() {}, sentinel
	}
	return builtin, sentinel
}

func TestServiceUsesExternalPluginWhenRunning(t *testing.T) {
	invoker := &fakeInvoker{
		status: "running",
		info:   plugin.Info{ID: "npa.files", Name: "外部文件插件"},
		result: map[string]any{
			"entries": []map[string]any{
				{"name": "a.txt", "path": "/tmp/a.txt", "size": 12, "mode": "-rw-r--r--", "mod_time_unix": 1700000000},
			},
		},
	}
	service := &Service{Builtin: NewBuiltin(nil)}
	service.AttachPlugin(NewPluginProvider(invoker))

	entries, err := service.List(context.Background(), "node-1", "/tmp")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(invoker.calls) != 1 || invoker.calls[0].method != "file.list" {
		t.Fatalf("expected one file.list call, got %+v", invoker.calls)
	}
	if invoker.calls[0].params.NodeID != "node-1" || invoker.calls[0].params.Path != "/tmp" {
		t.Fatalf("unexpected params: %+v", invoker.calls[0].params)
	}
	if len(entries) != 1 || entries[0].Name != "a.txt" || entries[0].Size != 12 {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	if info := service.PluginInfo(); info.ID != "npa.files" {
		t.Fatalf("expected external plugin info, got %+v", info)
	}
}

func TestServiceFallsBackToBuiltinWhenPluginMissing(t *testing.T) {
	invoker := &fakeInvoker{status: "runtime_missing"}
	builtin, sentinel := newBuiltinHit(t)
	service := &Service{Builtin: builtin}
	service.AttachPlugin(NewPluginProvider(invoker))

	_, err := service.List(context.Background(), "node-1", "/tmp")
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected builtin fallback error, got %v", err)
	}
	if len(invoker.calls) != 0 {
		t.Fatalf("plugin must not be called when it is not running: %+v", invoker.calls)
	}
	if info := service.PluginInfo(); info.ID != "npa.file" {
		t.Fatalf("expected builtin plugin info, got %+v", info)
	}
}

func TestServiceFallsBackWhenPluginRuntimeDies(t *testing.T) {
	invoker := &fakeInvoker{status: "running", err: plugin.ErrRuntimeMissing}
	builtin, sentinel := newBuiltinHit(t)
	service := &Service{Builtin: builtin}
	service.AttachPlugin(NewPluginProvider(invoker))

	_, err := service.List(context.Background(), "node-1", "/tmp")
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected builtin fallback after runtime loss, got %v", err)
	}
	if len(invoker.calls) != 1 {
		t.Fatalf("expected exactly one plugin attempt, got %+v", invoker.calls)
	}
}

func TestServiceSurfacesPluginError(t *testing.T) {
	boom := errors.New("plugin says no")
	invoker := &fakeInvoker{status: "running", err: boom}
	builtin, sentinel := newBuiltinHit(t)
	service := &Service{Builtin: builtin}
	service.AttachPlugin(NewPluginProvider(invoker))

	_, err := service.List(context.Background(), "node-1", "/tmp")
	if !errors.Is(err, boom) {
		t.Fatalf("expected plugin error to surface, got %v", err)
	}
	if errors.Is(err, sentinel) {
		t.Fatalf("plugin business error must not silently fall back to builtin")
	}
}

func TestPluginProviderPayloadRoundTrip(t *testing.T) {
	invoker := &fakeInvoker{status: "running", result: map[string]any{
		"content_base64": base64.StdEncoding.EncodeToString([]byte("hello plugin")),
	}}
	provider := NewPluginProvider(invoker)

	data, err := provider.Read(context.Background(), "node-1", "/tmp/a.txt")
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(data) != "hello plugin" {
		t.Fatalf("unexpected payload: %q", data)
	}
	if invoker.calls[0].method != "file.read" {
		t.Fatalf("unexpected method: %s", invoker.calls[0].method)
	}

	invoker.result = nil
	invoker.err = nil
	if err := provider.Write(context.Background(), "node-1", "/tmp/a.txt", []byte("hello plugin")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	writeCall := invoker.calls[1]
	if writeCall.method != "file.write" {
		t.Fatalf("unexpected method: %s", writeCall.method)
	}
	decoded, err := base64.StdEncoding.DecodeString(writeCall.params.ContentB64)
	if err != nil {
		t.Fatalf("plugin payload is not base64: %v", err)
	}
	if string(decoded) != "hello plugin" {
		t.Fatalf("unexpected write payload: %q", decoded)
	}
}

func TestPluginProviderRejectsOversizedPayload(t *testing.T) {
	invoker := &fakeInvoker{status: "running", result: map[string]any{
		"content_base64": base64.StdEncoding.EncodeToString(make([]byte, maxReadSize+1)),
	}}
	provider := NewPluginProvider(invoker)

	if _, err := provider.Read(context.Background(), "node-1", "/tmp/big.bin"); err == nil {
		t.Fatal("expected oversized plugin payload to be rejected")
	} else if !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("unexpected error: %v", err)
	} else if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("oversized payload must be marked as ErrPayloadTooLarge: %v", err)
	}
}

// 插件通道是 base64 JSON-RPC，有 4 MiB 上限；内置实现走流式通道不受限。
// 所以载荷超限要回退内置，而不是把大文件操作打断（升级包推送就走这条路径）。
func TestServiceFallsBackToBuiltinWhenPluginPayloadTooLarge(t *testing.T) {
	invoker := &fakeInvoker{status: "running", result: map[string]any{
		"content_base64": base64.StdEncoding.EncodeToString(make([]byte, maxReadSize+1)),
	}}
	builtin, sentinel := newBuiltinHit(t)
	service := &Service{Builtin: builtin}
	service.AttachPlugin(NewPluginProvider(invoker))

	if _, err := service.Read(context.Background(), "node-1", "/tmp/big.bin"); !errors.Is(err, sentinel) {
		t.Fatalf("expected builtin fallback on oversized payload, got %v", err)
	}
	if len(invoker.calls) != 1 {
		t.Fatalf("expected exactly one plugin attempt, got %+v", invoker.calls)
	}
}

func TestServiceFallsBackToBuiltinWhenWriteExceedsPluginLimit(t *testing.T) {
	invoker := &fakeInvoker{status: "running"}
	builtin, _ := newBuiltinHit(t)
	service := &Service{Builtin: builtin}
	service.AttachPlugin(NewPluginProvider(invoker))

	// Write 的 4 MiB 上限是策略上限（内置实现同样有），所以回退内置后
	// 报的还是同一个错；关键是不能把超限的数据塞进插件通道。
	err := service.Write(context.Background(), "node-1", "/tmp/big.bin", make([]byte, maxWriteSize+1))
	if err == nil || !strings.Contains(err.Error(), "exceeds write limit") {
		t.Fatalf("expected write limit error, got %v", err)
	}
	if len(invoker.calls) != 0 {
		t.Fatalf("oversized write must not reach the plugin channel: %+v", invoker.calls)
	}
}

// Upload 没有内置那样的 4 MiB 策略上限（升级包推送走这条路），
// 插件通道超限时必须回退内置的流式通道，否则大文件推不出去。
func TestServiceFallsBackToBuiltinWhenUploadExceedsPluginLimit(t *testing.T) {
	invoker := &fakeInvoker{status: "running"}
	builtin, sentinel := newBuiltinHit(t)
	service := &Service{Builtin: builtin}
	service.AttachPlugin(NewPluginProvider(invoker))

	_, err := service.Upload(context.Background(), "node-1", "/tmp/big.bin",
		strings.NewReader(strings.Repeat("x", maxWriteSize+1)))
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected builtin fallback on oversized upload, got %v", err)
	}
	if len(invoker.calls) != 0 {
		t.Fatalf("oversized upload must not reach the plugin channel: %+v", invoker.calls)
	}
}

func TestPluginProviderUploadReportsBytes(t *testing.T) {
	invoker := &fakeInvoker{status: "running", result: map[string]any{"bytes": 5}}
	provider := NewPluginProvider(invoker)

	written, err := provider.Upload(context.Background(), "node-1", "/tmp/a.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	if written != 5 {
		t.Fatalf("unexpected written bytes: %d", written)
	}
	if invoker.calls[0].method != "file.upload" {
		t.Fatalf("unexpected method: %s", invoker.calls[0].method)
	}
}

func TestBuiltinPluginMetadata(t *testing.T) {
	info := builtinMeta.Info()
	if info.ID != "npa.file" {
		t.Fatalf("unexpected builtin plugin id: %s", info.ID)
	}
	if info.Permission != "core.file" {
		t.Fatalf("unexpected permission: %s", info.Permission)
	}
	if len(info.Capabilities) == 0 {
		t.Fatal("builtin plugin must declare capabilities")
	}
}
