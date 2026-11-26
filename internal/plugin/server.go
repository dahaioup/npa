package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
)

// Handler 处理一个插件协议请求：返回结果即成功，返回 error 即回错误响应。
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// Server 是插件进程侧的服务端：读 NPA 插件协议请求（JSON-RPC 2.0 +
// Content-Length 分帧），分发给 Handler，再写回响应。
//
// 插件进程不需要自己实现分帧细节——这个 Server 就是给外部插件用的：
//
//	server := plugin.NewServer(os.Stdin, os.Stdout, handler)
//	if err := server.Serve(ctx); err != nil { ... }
type Server struct {
	reader  io.Reader
	writer  io.Writer
	handler Handler
	mu      sync.Mutex
	closed  bool
	stop    bool
}

// NewServer 构造插件服务端。
func NewServer(reader io.Reader, writer io.Writer, handler Handler) *Server {
	return &Server{reader: reader, writer: writer, handler: handler}
}

// Serve 循环处理请求直到输入流结束或上下文取消。
func (s *Server) Serve(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := ReadMessage(s.reader)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		var request requestMessage
		if err := json.Unmarshal(data, &request); err != nil {
			s.writeError(json.RawMessage(nil), -32700, "parse error: "+err.Error())
			continue
		}
		if request.Method == "" {
			continue
		}
		result, err := s.handler(ctx, request.Method, request.Params)
		if err != nil {
			s.writeError(requestID(request.ID), -32000, err.Error())
		} else {
			s.writeResult(requestID(request.ID), result)
		}
		// 插件收到 plugin.shutdown 后要退出进程：core 那边等的就是进程结束，
		// 一直不退会让 core 侧的关闭流程挂住。
		if s.stopped() {
			return nil
		}
	}
}

// Stop 请求服务端在当前请求处理完后停止服务（插件进程随即退出）。
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop = true
}

// stopped 判断是否收到了停止请求。
func (s *Server) stopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stop
}

// requestID 把请求 id 转成响应用的原始 JSON。
func requestID(id int64) json.RawMessage {
	if id == 0 {
		return nil
	}
	return json.RawMessage(strconv.FormatInt(id, 10))
}

// writeResult 写成功响应。
func (s *Server) writeResult(id json.RawMessage, result any) {
	encoded, err := json.Marshal(result)
	if err != nil {
		s.writeError(id, -32603, "encode result: "+err.Error())
		return
	}
	response := responseMessage{JSONRPC: "2.0", ID: id, Result: encoded}
	if err := s.write(response); err != nil {
		slog.Warn("plugin response write failed", "error", err)
	}
}

// writeError 写错误响应。
func (s *Server) writeError(id json.RawMessage, code int, message string) {
	response := responseMessage{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: message}}
	if err := s.write(response); err != nil {
		slog.Warn("plugin error response write failed", "error", err)
	}
}

// write 串行化写响应（并发调用时顺序不乱）。
func (s *Server) write(response responseMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("plugin server closed")
	}
	return WriteMessage(s.writer, response)
}
