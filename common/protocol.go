package common

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"
)

// 消息类型
const (
	MsgTypeAuth         = iota // 认证消息
	MsgTypeAuthResp            // 认证响应
	MsgTypeNewProxy            // 新建代理
	MsgTypeNewProxyResp        // 新建代理响应
	MsgTypeNewWorkConn         // 新工作连接
	MsgTypeHeartbeat    = 100  // 心跳检测
	MsgTypeHeartbeatAck = 101  // 心跳响应
)

// 超时和重试配置
const (
	DialTimeout       = 10 * time.Second // 连接超时
	HandshakeTimeout  = 5 * time.Second  // 握手超时
	HeartbeatTimeout  = 30 * time.Second // 心跳超时
	HeartbeatInterval = 15 * time.Second // 心跳间隔
)

// 协议错误定义
var (
	ErrAuthFailed = errors.New("authentication failed")
	ErrReadWrite  = errors.New("read/write error")
	ErrTimeout    = errors.New("connection timeout")
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

// ReadMessageWithTimeout 带超时读取消息
func ReadMessageWithTimeout(conn net.Conn, timeout time.Duration) (*Message, error) {
	// 设置读取超时
	conn.SetReadDeadline(time.Now().Add(timeout))
	defer conn.SetReadDeadline(time.Time{}) // 清除超时

	return ReadMessage(conn)
}

// WriteMessageWithTimeout 带超时写入消息
func WriteMessageWithTimeout(conn net.Conn, msgType byte, content []byte, timeout time.Duration) error {
	// 设置写入超时
	conn.SetWriteDeadline(time.Now().Add(timeout))
	defer conn.SetWriteDeadline(time.Time{}) // 清除超时

	return WriteMessage(conn, msgType, content)
}

// StartHeartbeat 启动心跳检测
func StartHeartbeat(conn net.Conn, stopCh <-chan struct{}) {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()

	lastReceived := time.Now()

	// 启动心跳接收处理
	go func() {
		for {
			// 非阻塞检查停止信号
			select {
			case <-stopCh:
				return
			default:
			}

			// 设置较长的超时等待心跳响应
			conn.SetReadDeadline(time.Now().Add(HeartbeatTimeout))
			msg, err := ReadMessage(conn)
			if err != nil {
				// 仅在不是关闭和超时的情况下记录错误
				if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
					if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
						Error("心跳响应读取失败: %v", err)
					}
				}
				continue
			}

			// 收到心跳响应
			if msg.Type == MsgTypeHeartbeat || msg.Type == MsgTypeHeartbeatAck {
				lastReceived = time.Now()
				// 如果收到心跳，回复心跳ACK
				if msg.Type == MsgTypeHeartbeat {
					WriteMessageWithTimeout(conn, MsgTypeHeartbeatAck, nil, time.Second)
				}
			} else {
				// 重置读取期限，让其他消息处理正常进行
				conn.SetReadDeadline(time.Time{})
				// 将非心跳消息传回主处理循环（此处需要自定义消息通道机制，此代码简化处理）
				break
			}
		}
	}()

	// 发送心跳
	for {
		select {
		case <-ticker.C:
			// 检查上次接收时间
			if time.Since(lastReceived) > HeartbeatTimeout {
				Error("心跳超时，关闭连接")
				conn.Close()
				return
			}

			// 发送心跳
			if err := WriteMessageWithTimeout(conn, MsgTypeHeartbeat, nil, time.Second); err != nil {
				Error("心跳发送失败: %v", err)
				conn.Close()
				return
			}
		case <-stopCh:
			return
		}
	}
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

// Transfer 在两个连接之间双向传输数据，返回错误通道
func Transfer(c1, c2 net.Conn) chan error {
	errCh := make(chan error, 2)

	go func() {
		err := CopyBuffer(c1, c2)
		errCh <- err
		c1.Close()
		c2.Close()
	}()

	go func() {
		err := CopyBuffer(c2, c1)
		errCh <- err
		c1.Close()
		c2.Close()
	}()

	return errCh
}
