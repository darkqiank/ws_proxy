package common

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
)

// 消息类型
const (
	MsgTypeAuth         = iota // 认证消息
	MsgTypeAuthResp            // 认证响应
	MsgTypeNewProxy            // 新建代理
	MsgTypeNewProxyResp        // 新建代理响应
	MsgTypeNewWorkConn         // 新工作连接
)

// 协议错误定义
var (
	ErrAuthFailed = errors.New("authentication failed")
	ErrReadWrite  = errors.New("read/write error")
)

// Message 消息结构
type Message struct {
	Type    byte   // 消息类型
	Length  uint32 // 数据长度
	Content []byte // 消息内容
}

// WriteMessage 写入消息
func WriteMessage(conn net.Conn, msgType byte, content []byte) error {
	// 消息头: 1字节类型 + 4字节长度
	header := make([]byte, 5)
	header[0] = msgType
	binary.BigEndian.PutUint32(header[1:], uint32(len(content)))

	// 写入消息头
	if _, err := conn.Write(header); err != nil {
		return err
	}

	// 写入消息内容
	if len(content) > 0 {
		if _, err := conn.Write(content); err != nil {
			return err
		}
	}

	return nil
}

// ReadMessage 读取消息
func ReadMessage(conn net.Conn) (*Message, error) {
	// 读取消息头
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}

	// 解析消息头
	msgType := header[0]
	msgLen := binary.BigEndian.Uint32(header[1:])

	// 读取消息内容
	var content []byte
	if msgLen > 0 {
		content = make([]byte, msgLen)
		if _, err := io.ReadFull(conn, content); err != nil {
			return nil, err
		}
	}

	return &Message{
		Type:    msgType,
		Length:  msgLen,
		Content: content,
	}, nil
}

// AuthRequest 认证请求
type AuthRequest struct {
	Token    string `json:"token"`
	ClientID string `json:"client_id"` // 客户端唯一标识
}

// AuthResponse 认证响应
type AuthResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// NewProxyRequest 新建代理请求
type NewProxyRequest struct {
	Name       string `json:"name"`
	RemotePort int    `json:"remote_port"`
	ClientID   string `json:"client_id"` // 客户端唯一标识
}

// NewProxyResponse 新建代理响应
type NewProxyResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// NewWorkConnRequest 新工作连接请求
type NewWorkConnRequest struct {
	ProxyName string `json:"proxy_name"`
	ClientID  string `json:"client_id"` // 客户端唯一标识
}

// CopyBuffer 在两个连接之间复制数据
func CopyBuffer(dst, src net.Conn) error {
	buf := make([]byte, 32*1024)
	_, err := io.CopyBuffer(dst, src, buf)
	return err
}

// Transfer 在两个连接之间双向传输数据
func Transfer(c1, c2 net.Conn) {
	go func() {
		CopyBuffer(c1, c2)
		c1.Close()
		c2.Close()
	}()

	CopyBuffer(c2, c1)
	c1.Close()
	c2.Close()
}
