package file

import (
	"context"
	"io"
	"time"

	"github.com/npa/npa/internal/model"
)

// Provider 是文件能力的插件契约。
//
// 两种实现：
//   - Builtin：内置插件（纯 Go，随 core 编译进来），走节点 HTTP 文件面 →
//     跨域代理 → SFTP 三级选路。
//   - PluginProvider：外部插件进程（plugins/file 下的可执行文件），
//     通过 JSON-RPC 方法 file.* 暴露同一套能力。
//
// 契约语义与底层链路无关：每次调用都带 nodeID，由实现方决定怎么抵达那个节点。
// 调用方（MCP file.* 工具、Web /api/nodes/{id}/files/*）只依赖本接口，
// 因此换实现不改变任何对外接口。
type Provider interface {
	// List 列出远端目录下的条目（目录在前、按名排序由实现方保证）。
	List(ctx context.Context, nodeID, remotePath string) ([]model.FileEntry, error)
	// Stat 读远端路径的元信息。
	Stat(ctx context.Context, nodeID, remotePath string) (model.FileEntry, error)
	// Read 读整个远端文件内容，受大小上限约束。
	Read(ctx context.Context, nodeID, remotePath string) ([]byte, error)
	// OpenRead 打开远端文件流，调用方负责 Close。
	OpenRead(ctx context.Context, nodeID, remotePath string) (io.ReadCloser, error)
	// Write 覆写远端文件。
	Write(ctx context.Context, nodeID, remotePath string, data []byte) error
	// Upload 把本地数据流写到远端文件，返回实际写入字节数。
	Upload(ctx context.Context, nodeID, remotePath string, source io.Reader) (int64, error)
	// Remove 删除远端文件或目录。
	Remove(ctx context.Context, nodeID, remotePath string) error
	// Mkdir 创建远端目录（含父目录）。
	Mkdir(ctx context.Context, nodeID, remotePath string) error
	// Rename 改远端路径名。
	Rename(ctx context.Context, nodeID, oldPath, newPath string) error
	// Touch 建空文件（已存在则截断）。
	Touch(ctx context.Context, nodeID, remotePath string) error
	// TouchTime 设置远端文件的时间戳。
	TouchTime(ctx context.Context, nodeID, remotePath string, modTime time.Time) error
	// CreateReader 打开远端文件的只读流（不负责关闭）。
	CreateReader(ctx context.Context, nodeID, remotePath string) (io.Reader, error)
}

// fileEntry 是目录项在两条通道上的共同形状：节点 HTTP 文件面
// （internal/file/face.go）与文件插件协议（file.list / file.stat）都用它。
type fileEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	Mode        string `json:"mode"`
	ModTimeUnix int64  `json:"mod_time_unix"`
	IsDir       bool   `json:"is_dir"`
}

func (e fileEntry) model() model.FileEntry {
	return model.FileEntry{
		Name: e.Name, Path: e.Path, Size: e.Size, Mode: e.Mode,
		ModTime: time.Unix(e.ModTimeUnix, 0).UTC(), IsDir: e.IsDir,
	}
}

var (
	_ Provider = (*Builtin)(nil)
	_ Provider = (*PluginProvider)(nil)
	_ Provider = (*Service)(nil)
)
