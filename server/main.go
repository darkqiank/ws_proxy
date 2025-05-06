package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/gotunnel/common"
)

var (
	// 命令行参数
	configFile = flag.String("config", "config.json", "配置文件路径")

	// 全局配置
	config *common.ServerConfig

	// 代理管理
	proxyMutex sync.Mutex
	proxies    = make(map[string]*Proxy)

	// 客户端管理
	clientMutex sync.Mutex
	clients     = make(map[string][]*Proxy) // 客户端地址 -> 代理列表
)

// Proxy 代理信息
type Proxy struct {
	Name         string
	RemotePort   int
	Listener     net.Listener
	Conns        chan net.Conn
	ClientAddr   string        // 关联的客户端地址
	LastActivity time.Time     // 最后活动时间
	Done         chan struct{} // 关闭信号
}

func main() {
	// 解析命令行参数
	flag.Parse()

	// 加载配置
	var err error
	config, err = common.LoadServerConfig(*configFile)
	if err != nil {
		common.Error("加载配置失败: %v", err)
		return
	}
	common.Info("加载配置成功")

	// 启动控制服务
	address := fmt.Sprintf("%s:%d", config.BindAddr, config.BindPort)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		common.Error("启动服务失败: %v", err)
		return
	}
	defer listener.Close()

	common.Info("服务已启动，监听地址: %s", address)

	// 处理连接
	for {
		conn, err := listener.Accept()
		if err != nil {
			common.Error("接受连接失败: %v", err)
			continue
		}

		go handleControlConnection(conn)
	}
}

// 处理控制连接
func handleControlConnection(conn net.Conn) {
	defer func() {
		conn.Close()
		// 客户端断开时清理相关代理
		cleanupClientProxies(conn.RemoteAddr().String())
	}()

	clientAddr := conn.RemoteAddr().String()
	common.Info("收到新的控制连接: %s", clientAddr)

	// 处理认证
	if !handleAuth(conn) {
		return
	}

	// 处理控制消息
	for {
		msg, err := common.ReadMessage(conn)
		if err != nil {
			common.Error("读取消息失败: %v", err)
			break
		}

		switch msg.Type {
		case common.MsgTypeNewProxy:
			handleNewProxy(conn, msg)
		case common.MsgTypeNewWorkConn:
			handleNewWorkConn(conn, msg)
		default:
			common.Error("未知消息类型: %d", msg.Type)
		}
	}
}

// 清理客户端关联的代理
func cleanupClientProxies(clientAddr string) {
	clientMutex.Lock()
	proxiesForClient, exists := clients[clientAddr]
	if exists {
		delete(clients, clientAddr)
	}
	clientMutex.Unlock()

	if !exists {
		return
	}

	common.Info("客户端断开连接，清理相关代理: %s", clientAddr)

	// 关闭该客户端的所有代理
	for _, proxy := range proxiesForClient {
		proxyMutex.Lock()
		closeProxy(proxy)
		proxyMutex.Unlock()
	}
}

// 关闭代理
func closeProxy(proxy *Proxy) {
	if proxy == nil {
		return
	}

	name := proxy.Name
	if _, exists := proxies[name]; exists {
		common.Info("关闭代理: %s", name)
		// 关闭监听器
		if proxy.Listener != nil {
			proxy.Listener.Close()
		}
		// 发送关闭信号
		close(proxy.Done)
		// 删除代理
		delete(proxies, name)
	}
}

// 处理认证
func handleAuth(conn net.Conn) bool {
	msg, err := common.ReadMessage(conn)
	if err != nil {
		common.Error("读取认证消息失败: %v", err)
		return false
	}

	if msg.Type != common.MsgTypeAuth {
		common.Error("预期认证消息，收到类型: %d", msg.Type)
		return false
	}

	var authReq common.AuthRequest
	if err := json.Unmarshal(msg.Content, &authReq); err != nil {
		common.Error("解析认证消息失败: %v", err)
		return false
	}

	// 验证Token
	var authResp common.AuthResponse
	if authReq.Token == config.Token {
		authResp.Success = true
		common.Info("客户端认证成功: %s", conn.RemoteAddr().String())
	} else {
		authResp.Success = false
		authResp.Error = "invalid token"
		common.Error("客户端认证失败: %s", conn.RemoteAddr().String())
	}

	// 发送认证响应
	respData, err := json.Marshal(authResp)
	if err != nil {
		common.Error("序列化认证响应失败: %v", err)
		return false
	}

	if err := common.WriteMessage(conn, common.MsgTypeAuthResp, respData); err != nil {
		common.Error("发送认证响应失败: %v", err)
		return false
	}

	// 认证成功后初始化客户端记录
	if authResp.Success {
		clientMutex.Lock()
		clients[conn.RemoteAddr().String()] = []*Proxy{}
		clientMutex.Unlock()
	}

	return authResp.Success
}

// 处理新建代理请求
func handleNewProxy(conn net.Conn, msg *common.Message) {
	var req common.NewProxyRequest
	if err := json.Unmarshal(msg.Content, &req); err != nil {
		common.Error("解析新建代理请求失败: %v", err)
		return
	}

	clientAddr := conn.RemoteAddr().String()
	common.Info("收到新建代理请求: %s, 远程端口: %d, 客户端: %s", req.Name, req.RemotePort, clientAddr)

	var resp common.NewProxyResponse
	respData, err := func() ([]byte, error) {
		proxyMutex.Lock()
		defer proxyMutex.Unlock()

		// 检查代理是否已存在，如果存在则关闭旧代理
		if existingProxy, exists := proxies[req.Name]; exists {
			// 如果是同一个客户端，允许覆盖
			if existingProxy.ClientAddr == clientAddr {
				common.Info("同一客户端重新注册代理: %s", req.Name)
				closeProxy(existingProxy)
			} else {
				// 不同客户端尝试注册同名代理，拒绝
				common.Error("不同客户端尝试注册同名代理: %s", req.Name)
				resp.Success = false
				resp.Error = "proxy already exists and owned by another client"
				return json.Marshal(resp)
			}
		}

		// 创建监听器
		address := fmt.Sprintf("%s:%d", config.BindAddr, req.RemotePort)
		listener, err := net.Listen("tcp", address)
		if err != nil {
			common.Error("创建监听器失败: %v", err)
			resp.Success = false
			resp.Error = fmt.Sprintf("failed to listen on port %d: %v", req.RemotePort, err)
			return json.Marshal(resp)
		}

		// 创建代理
		proxy := &Proxy{
			Name:         req.Name,
			RemotePort:   req.RemotePort,
			Listener:     listener,
			Conns:        make(chan net.Conn, 100),
			ClientAddr:   clientAddr,
			LastActivity: time.Now(),
			Done:         make(chan struct{}),
		}
		proxies[req.Name] = proxy

		// 将代理添加到客户端关联列表
		clientMutex.Lock()
		clients[clientAddr] = append(clients[clientAddr], proxy)
		clientMutex.Unlock()

		// 启动代理监听
		go handleProxyConnections(proxy)

		resp.Success = true
		common.Info("代理创建成功: %s, 远程端口: %d", req.Name, req.RemotePort)
		return json.Marshal(resp)
	}()

	if err != nil {
		common.Error("序列化代理响应失败: %v", err)
		return
	}

	if err := common.WriteMessage(conn, common.MsgTypeNewProxyResp, respData); err != nil {
		common.Error("发送代理响应失败: %v", err)
	}
}

// 处理代理连接
func handleProxyConnections(proxy *Proxy) {
	defer func() {
		proxyMutex.Lock()
		closeProxy(proxy)
		proxyMutex.Unlock()
	}()

	// 连接接收器
	connAcceptor := make(chan net.Conn)
	connError := make(chan error)

	// 启动接收连接的goroutine
	go func() {
		for {
			conn, err := proxy.Listener.Accept()
			if err != nil {
				connError <- err
				return
			}
			connAcceptor <- conn
		}
	}()

	for {
		select {
		case conn := <-connAcceptor:
			// 收到新连接
			common.Info("代理 %s 收到新连接: %s", proxy.Name, conn.RemoteAddr().String())
			proxy.LastActivity = time.Now()
			proxy.Conns <- conn
		case err := <-connError:
			// 监听器错误
			common.Error("代理 %s 监听错误: %v", proxy.Name, err)
			return
		case <-proxy.Done:
			// 收到关闭信号
			common.Info("代理 %s 收到关闭信号", proxy.Name)
			return
		}
	}
}

// 处理新工作连接请求
func handleNewWorkConn(conn net.Conn, msg *common.Message) {
	var req common.NewWorkConnRequest
	if err := json.Unmarshal(msg.Content, &req); err != nil {
		common.Error("解析新工作连接请求失败: %v", err)
		return
	}

	clientAddr := conn.RemoteAddr().String()
	common.Info("收到新工作连接请求: %s, 客户端: %s", req.ProxyName, clientAddr)

	proxyMutex.Lock()
	proxy, exists := proxies[req.ProxyName]

	// 确保工作连接来自正确的客户端
	if exists && proxy.ClientAddr != clientAddr {
		common.Error("工作连接请求来自错误的客户端: %s, 期望: %s", clientAddr, proxy.ClientAddr)
		exists = false
	}
	proxyMutex.Unlock()

	if !exists {
		common.Error("代理不存在或不属于该客户端: %s", req.ProxyName)
		return
	}

	// 更新活动时间
	proxy.LastActivity = time.Now()

	// 等待用户连接
	userConn, ok := <-proxy.Conns
	if !ok {
		common.Error("代理连接通道已关闭: %s", req.ProxyName)
		return
	}

	// 数据转发
	common.Info("开始代理数据传输: %s", req.ProxyName)
	common.Transfer(conn, userConn)
}
