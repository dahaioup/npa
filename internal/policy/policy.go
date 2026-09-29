package policy

import (
	"context"

	apperrors "github.com/npa/npa/internal/errors"
)

type Capability uint64

const (
	CapSSH Capability = 1 << iota
	CapFile
	CapTunnel
	CapScreenshot
	CapMouse
	CapDiscovery
	CapRelay
	CapBase Capability = 0
)

type Source string

const (
	SourceWeb Source = "web"
	SourceMCP Source = "mcp"
)

type Getter interface {
	MCPCapabilities() map[string]bool
}

type contextKey struct{}

func WithSource(ctx context.Context, source Source) context.Context {
	return context.WithValue(ctx, contextKey{}, source)
}

func SourceFrom(ctx context.Context) Source {
	if source, ok := ctx.Value(contextKey{}).(Source); ok {
		return source
	}
	return SourceWeb
}

func (c Capability) String() string {
	switch c {
	case CapSSH:
		return "ssh"
	case CapFile:
		return "file"
	case CapTunnel:
		return "tunnel"
	case CapScreenshot:
		return "screenshot"
	case CapMouse:
		return "mouse"
	case CapDiscovery:
		return "discovery"
	case CapRelay:
		return "relay"
	default:
		return "base"
	}
}

func Check(ctx context.Context, getter Getter, capability Capability) error {
	if SourceFrom(ctx) != SourceMCP || capability == CapBase {
		return nil
	}
	name := capability.String()
	if !getter.MCPCapabilities()[name] {
		return apperrors.New(apperrors.PermissionDenied, "MCP capability '"+name+"' is disabled")
	}
	return nil
}
