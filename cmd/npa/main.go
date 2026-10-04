package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/npa/npa/internal/mcp"
	"github.com/npa/npa/internal/service"
	"github.com/npa/npa/internal/state"
	"github.com/npa/npa/internal/web"
)

var version = service.Version

type mcpSupervisor struct {
	handler http.Handler
	mu      sync.Mutex
	server  *http.Server
	scope   string
}

func mcpAddress(scope string) string {
	if scope == "lan" {
		return "0.0.0.0:56701"
	}
	return "127.0.0.1:56701"
}

func (s *mcpSupervisor) currentScope() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scope
}

func (s *mcpSupervisor) restart(scope string) error {
	s.mu.Lock()
	if s.scope == scope && s.server != nil {
		s.mu.Unlock()
		return nil
	}
	old := s.server
	s.server = nil
	s.scope = scope
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	address := mcpAddress(scope)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: s.handler, ReadHeaderTimeout: 10 * time.Second}
	s.mu.Lock()
	s.server = server
	s.mu.Unlock()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("MCP server stopped", "address", address, "error", err)
		}
	}()
	slog.Info("MCP server listening", "address", address)
	return nil
}

func (s *mcpSupervisor) shutdown(ctx context.Context) error {
	s.mu.Lock()
	server := s.server
	s.server = nil
	s.scope = ""
	s.mu.Unlock()
	if server == nil {
		return nil
	}
	return server.Shutdown(ctx)
}

func main() {
	dataDir := flag.String("data-dir", "", "internal state directory (default ~/.npa)")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	level := slog.LevelInfo
	switch strings.ToLower(*logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	store, err := state.Open(*dataDir)
	if err != nil {
		slog.Error("cannot open state database", "error", err)
		os.Exit(1)
	}
	app := service.New(store)
	mcpManager := mcp.NewManager(app)
	mcpHTTPHandler := mcpManager.Handler()
	mcpSupervisor := &mcpSupervisor{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if app.Settings()["mcp_enabled"] != "true" {
			http.Error(w, "MCP service is disabled", http.StatusForbidden)
			return
		}
		mcpHTTPHandler.ServeHTTP(w, r)
	})}
	app.OnSettingsChanged = func() {
		mcpManager.Refresh()
		if err := mcpSupervisor.restart(app.Settings()["mcp_bind_scope"]); err != nil {
			slog.Error("cannot restart MCP listener", "scope", app.Settings()["mcp_bind_scope"], "error", err)
		}
	}
	app.ApplyRuntimeSettings(context.Background())
	go func() {
		if err := app.Plugin.Start(context.Background()); err != nil {
			slog.Warn("desktop plugin unavailable; using embedded fallback", "error", err)
		}
	}()
	if days := app.Settings()["audit_retention_days"]; days != "" {
		_ = app.Audit.Prune(parseInt(days))
	}

	webServer := &http.Server{
		Addr:              "127.0.0.1:56700",
		Handler:           web.New(app, mcpManager).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	webErr := make(chan error, 1)
	go func() {
		slog.Info("Web server listening", "address", "http://127.0.0.1:56700")
		webErr <- webServer.ListenAndServe()
	}()

	if app.Settings()["restore_tunnels"] == "true" {
		go func() {
			if err := app.Tunnels.Restore(context.Background()); err != nil {
				slog.Warn("tunnel restore failed", "error", err)
			}
		}()
	}
	slog.Info("NPA ready", "version", version)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case <-stop:
	case err := <-webErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("web server stopped", "error", err)
		}
	}
	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = mcpSupervisor.shutdown(ctx)
	_ = webServer.Shutdown(ctx)
	app.Close()
}

func parseInt(value string) int {
	var result int
	_, _ = fmt.Sscanf(value, "%d", &result)
	return result
}
