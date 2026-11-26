// Package peer 实现 NPA 节点之间的反向接入：子网侧（下级）主动发现并注册到上级，
// 与上级保持一条长连接和一组空闲数据连接，使上级可以随时反向联系子网节点。
//
// 所有节点的 SSH 都只作为客户端使用，NPA 不实现 SSH 服务端：NPA 节点之间的转发
// 由本包实现，NPA 节点到 SSH 服务的转发才走 SSH；两者可以串联（链式 via）。
package peer

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// maxFrameSize 限制单个 WebSocket 帧，避免异常对端造成内存放大。
const maxFrameSize = 4 << 20

// WSConn 把 WebSocket 适配成 net.Conn：每次 Write 产出一个二进制帧，
// Read 跨帧按字节流读取（gorilla 只提供 ReadMessage/WriteMessage，不满足 net.Conn）。
type WSConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	reader  io.Reader
}

func NewWSConn(conn *websocket.Conn) *WSConn {
	conn.SetReadLimit(maxFrameSize)
	return &WSConn{conn: conn}
}

func (w *WSConn) Read(p []byte) (int, error) {
	for {
		if w.reader == nil {
			messageType, reader, err := w.conn.NextReader()
			if err != nil {
				return 0, err
			}
			if messageType != websocket.BinaryMessage && messageType != websocket.TextMessage {
				continue
			}
			w.reader = reader
		}
		n, err := w.reader.Read(p)
		if n > 0 {
			if err == io.EOF {
				w.reader = nil
			}
			return n, nil
		}
		if err == io.EOF {
			w.reader = nil
			continue
		}
		if err != nil {
			return 0, err
		}
	}
}

func (w *WSConn) Write(p []byte) (int, error) {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if err := w.conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *WSConn) Close() error                       { return w.conn.Close() }
func (w *WSConn) LocalAddr() net.Addr                { return w.conn.LocalAddr() }
func (w *WSConn) RemoteAddr() net.Addr               { return w.conn.RemoteAddr() }
func (w *WSConn) SetReadDeadline(t time.Time) error  { return w.conn.SetReadDeadline(t) }
func (w *WSConn) SetWriteDeadline(t time.Time) error { return w.conn.SetWriteDeadline(t) }

func (w *WSConn) SetDeadline(t time.Time) error {
	if err := w.SetReadDeadline(t); err != nil {
		return err
	}
	return w.SetWriteDeadline(t)
}

var _ net.Conn = (*WSConn)(nil)
