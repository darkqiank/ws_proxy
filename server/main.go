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
	clients     = make(map[string][]*Proxy) // 客户端ID -> 代理列表

	// 控制连接映射
	controlsMutex sync.Mutex
	controlConns  = make(map[string]net.Conn) // 客户端ID -> 控制连接
)

// Proxy 代理信息
type Proxy struct {
	Name         string
	RemotePort   int
	Listener     net.Listener
	Conns        chan net.Conn
	ClientID     string        // 关联的客户端ID
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

		go handleConnection(conn)
	}
}

// 处理新连接
func handleConnection(conn net.Conn) {
	// 读取第一条消息确定连接类型
	msg, err := common.ReadMessage(conn)
	if err != nil {
		common.Error("读取初始消息失败: %v", err)
		conn.Close()
		return
	}

	// 只接受认证类型的消息作为初始消息
	if msg.Type != common.MsgTypeAuth {
		common.Error("初始消息不是认证类型: %d", msg.Type)
		conn.Close()
		return
	}

	// 解析认证请求
	var authReq common.AuthRequest
	if err := json.Unmarshal(msg.Content, &authReq); err != nil {
		common.Error("解析认证消息失败: %v", err)
		conn.Close()
		return
	}

	// 获取客户端ID
	clientID := authReq.ClientID
	if clientID == "" {
		common.Error("客户端未提供ID")
		conn.Close()
		return
	}

	// 验证Token
	var authResp common.AuthResponse
	if authReq.Token == config.Token {
		authResp.Success = true
	} else {
		authResp.Success = false
		authResp.Error = "invalid token"
		common.Error("客户端认证失败: %s (IP: %s)", clientID, conn.RemoteAddr().String())

		// 发送认证失败响应并关闭连接
		respData, _ := json.Marshal(authResp)
		common.WriteMessage(conn, common.MsgTypeAuthResp, respData)
		conn.Close()
		return
	}

	// 发送认证成功响应
	respData, err := json.Marshal(authResp)
	if err != nil {
		common.Error("序列化认证响应失败: %v", err)
		conn.Close()
		return
	}

	if err := common.WriteMessage(conn, common.MsgTypeAuthResp, respData); err != nil {
		common.Error("发送认证响应失败: %v", err)
		conn.Close()
		return
	}

	// 读取下一条消息确定具体操作
	msg, err = common.ReadMessage(conn)
	if err != nil {
		common.Error("读取操作消息失败: %v", err)
		conn.Close()
		return
	}

	switch msg.Type {
	case common.MsgTypeNewProxy:
		// 控制连接: 处理代理注册
		common.Info("客户端认证成功: %s (IP: %s) - 控制连接", clientID, conn.RemoteAddr().String())
		handleControlConnection(conn, msg, clientID)
	case common.MsgTypeNewWorkConn:
		// 工作连接: 处理数据转发
		common.Info("客户端认证成功: %s (IP: %s) - 工作连接", clientID, conn.RemoteAddr().String())
		handleWorkConnection(conn, msg, clientID)
	default:
		common.Error("未知操作类型: %d", msg.Type)
		conn.Close()
	}
}

// 处理控制连接
func handleControlConnection(conn net.Conn, initialMsg *common.Message, clientID string) {
	// 注册控制连接
	controlsMutex.Lock()
	// 如果已有控制连接，关闭旧连接
	if oldConn, exists := controlConns[clientID]; exists {
		common.Info("关闭客户端 %s 的旧控制连接", clientID)
		oldConn.Close()
	}
	controlConns[clientID] = conn
	controlsMutex.Unlock()

	// 初始化客户端记录
	clientMutex.Lock()
	if _, exists := clients[clientID]; !exists {
		clients[clientID] = []*Proxy{}
	}
	clientMutex.Unlock()

	// 先处理初始消息(新建代理请求)
	handleNewProxy(conn, initialMsg, clientID)

	// 持续监听控制连接的消息
	defer func() {
		conn.Close()

		// 从控制连接映射中移除
		controlsMutex.Lock()
		delete(controlConns, clientID)
		controlsMutex.Unlock()

		// 清理该客户端的所有代理
		cleanupClientProxies(clientID)
	}()

	for {
		msg, err := common.ReadMessage(conn)
		if err != nil {
			common.Error("读取控制消息失败: %v", err)
			break
		}

		switch msg.Type {
		case common.MsgTypeNewProxy:
			handleNewProxy(conn, msg, clientID)
		default:
			common.Error("控制连接收到未知类型消息: %d", msg.Type)
		}
	}
}

// 处理工作连接
func handleWorkConnection(conn net.Conn, initialMsg *common.Message, clientID string) {
	// 解析工作连接请求
	var req common.NewWorkConnRequest
	if err := json.Unmarshal(initialMsg.Content, &req); err != nil {
		common.Error("解析工作连接请求失败: %v", err)
		conn.Close()
		return
	}

	// 验证请求中的客户端ID
	if req.ClientID != clientID {
		common.Error("工作连接请求中的客户端ID不匹配: %s != %s", req.ClientID, clientID)
		conn.Close()
		return
	}

	proxyName := req.ProxyName
	common.Info("收到新工作连接请求: %s, 客户端: %s", proxyName, clientID)

	// 获取代理
	proxyMutex.Lock()
	proxy, exists := proxies[proxyName]

	// 确保工作连接来自正确的客户端
	if exists && proxy.ClientID != clientID {
		common.Error("工作连接请求的代理属于其他客户端: %s, 期望: %s", clientID, proxy.ClientID)
		exists = false
	}
	proxyMutex.Unlock()

	if !exists {
		common.Error("代理不存在或不属于该客户端: %s", proxyName)
		conn.Close()
		return
	}

	// 更新活动时间
	proxy.LastActivity = time.Now()

	// 等待用户连接
	common.Info("工作连接等待用户连接: %s", proxyName)
	userConn, ok := <-proxy.Conns
	if !ok {
		common.Error("代理连接通道已关闭: %s", proxyName)
		conn.Close()
		return
	}

	// 数据转发
	common.Info("开始代理数据传输: %s", proxyName)
	common.Transfer(conn, userConn)
}

// 清理客户端关联的代理
func cleanupClientProxies(clientID string) {
	clientMutex.Lock()
	proxiesForClient, exists := clients[clientID]
	if exists {
		delete(clients, clientID)
	}
	clientMutex.Unlock()

	if !exists || len(proxiesForClient) == 0 {
		return
	}

	common.Info("客户端断开连接，清理相关代理: %s", clientID)

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

// 处理新建代理请求
func handleNewProxy(conn net.Conn, msg *common.Message, clientID string) {
	var req common.NewProxyRequest
	if err := json.Unmarshal(msg.Content, &req); err != nil {
		common.Error("解析新建代理请求失败: %v", err)
		return
	}

	// 验证客户端ID匹配
	if req.ClientID != clientID {
		common.Error("代理请求中的客户端ID不匹配: %s != %s", req.ClientID, clientID)
		return
	}

	common.Info("收到新建代理请求: %s, 远程端口: %d, 客户端: %s", req.Name, req.RemotePort, clientID)

	var resp common.NewProxyResponse
	respData, err := func() ([]byte, error) {
		proxyMutex.Lock()
		defer proxyMutex.Unlock()

		// 检查代理是否已存在，如果存在则关闭旧代理
		if existingProxy, exists := proxies[req.Name]; exists {
			// 如果是同一个客户端，允许覆盖
			if existingProxy.ClientID == clientID {
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
			ClientID:     clientID,
			LastActivity: time.Now(),
			Done:         make(chan struct{}),
		}
		proxies[req.Name] = proxy

		// 将代理添加到客户端关联列表
		clientMutex.Lock()
		clients[clientID] = append(clients[clientID], proxy)
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
