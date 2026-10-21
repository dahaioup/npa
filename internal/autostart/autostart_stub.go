//go:build !windows && !darwin && !linux

package autostart

// 其他平台（freebsd 等）暂无实现，UI 会显示"当前平台不支持"。
func platformStatus() (Status, error) { return Status{Supported: false}, ErrNotSupported }

func platformEnable(string) error { return ErrNotSupported }

func platformDisable() error { return ErrNotSupported }
