package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gotunnel/common"
)

var (
	// 命令行参数
	configFile = flag.String("config", "config.json", "配置文件路径")

	// 全局配置
	config *common.ClientConfig

	// 客户端ID
	clientID string

	// 工作连接管理
	controlConn net.Conn
	workConnCh  = make(map[string]chan net.Conn)
	workConnMu  sync.Mutex

	// 重连相关
	reconnectCh = make(chan struct{})
	exitCh      = make(chan struct{})
)

// 生成随机客户端ID
func generateClientID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func main() {
	// 解析命令行参数
	flag.Parse()

	// 加载配置
	var err error
	config, err = common.LoadClientConfig(*configFile)
	if err != nil {
		common.Error("加载配置失败: %v", err)
		return
	}
	common.Info("加载配置成功")

	// 生成客户端ID
	clientID = generateClientID()
	common.Info("客户端ID: %s", clientID)

	// 初始化通道
	for _, proxyConfig := range config.Proxies {
		workConnCh[proxyConfig.Name] = make(chan net.Conn, 10)
	}

	// 设置上下文，用于优雅退出
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 处理信号量
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		common.Info("接收到退出信号，正在关闭...")
		cancel()
	}()

	// 启动客户端
	go startClient(ctx)

	// 等待退出
	<-ctx.Done()
	close(exitCh)
	common.Info("客户端已退出")
}

// 启动客户端
func startClient(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			if err := runClient(ctx); err != nil {
				common.Error("客户端运行出错: %v", err)
				// 等待一段时间后重连
				select {
				case <-time.After(5 * time.Second):
					common.Info("尝试重连...")
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// 运行客户端
func runClient(ctx context.Context) error {
	var err error

	// 连接服务端
	address := fmt.Sprintf("%s:%d", config.ServerAddr, config.ServerPort)

	// 设置连接超时
	dialer := &net.Dialer{
		Timeout: common.DialTimeout,
	}

	controlConn, err = dialer.Dial("tcp", address)
	if err != nil {
		return fmt.Errorf("连接服务端失败: %v", err)
	}
	defer func() {
		controlConn.Close()
		// 通知所有工作连接重新连接
		select {
		case reconnectCh <- struct{}{}:
		default:
		}
	}()

	common.Info("已连接到服务端: %s", address)

	// 客户端认证
	if err = authenticate(controlConn); err != nil {
		return fmt.Errorf("认证失败: %v", err)
	}

	// 注册所有代理
	for _, proxyConfig := range config.Proxies {
		if err = registerProxy(controlConn, proxyConfig); err != nil {
			return fmt.Errorf("注册代理 %s 失败: %v", proxyConfig.Name, err)
		}
	}

	// 启动工作连接处理
	var wg sync.WaitGroup
	for _, proxyConfig := range config.Proxies {
		wg.Add(1)
		go func(proxyConfig common.ProxyConfig) {
			defer wg.Done()
			startLocalProxy(ctx, proxyConfig)
		}(proxyConfig)
	}

	// 创建一个通道用于停止心跳
	heartbeatStopCh := make(chan struct{})
	defer close(heartbeatStopCh)

	// 启动心跳检测
	go common.StartHeartbeat(controlConn, heartbeatStopCh)

	// 监听控制连接消息
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			msg, err := common.ReadMessageWithTimeout(controlConn, common.HeartbeatTimeout)
			if err != nil {
				// 如果是超时或EOF，表示连接已断开
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					return fmt.Errorf("控制连接读取超时")
				}
				if err == io.EOF {
					return fmt.Errorf("控制连接断开")
				}
				return fmt.Errorf("控制连接读取错误: %v", err)
			}

			// 处理来自服务端的消息
			switch msg.Type {
			case common.MsgTypeHeartbeat, common.MsgTypeHeartbeatAck:
				// 心跳消息由心跳处理协程处理
				continue
			default:
				common.Error("收到未知类型的控制消息: %d", msg.Type)
			}
		}
	}
}

// 客户端认证
func authenticate(conn net.Conn) error {
	// 发送认证请求
	authReq := common.AuthRequest{
		Token:    config.Token,
		ClientID: clientID,
	}
	reqData, err := json.Marshal(authReq)
	if err != nil {
		return fmt.Errorf("序列化认证请求失败: %v", err)
	}

	if err := common.WriteMessage(conn, common.MsgTypeAuth, reqData); err != nil {
		return fmt.Errorf("发送认证请求失败: %v", err)
	}

	// 接收认证响应
	msg, err := common.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("读取认证响应失败: %v", err)
	}

	if msg.Type != common.MsgTypeAuthResp {
		return fmt.Errorf("预期认证响应，收到类型: %d", msg.Type)
	}

	var authResp common.AuthResponse
	if err := json.Unmarshal(msg.Content, &authResp); err != nil {
		return fmt.Errorf("解析认证响应失败: %v", err)
	}

	if !authResp.Success {
		return fmt.Errorf("认证失败: %s", authResp.Error)
	}

	common.Info("认证成功")
	return nil
}

// 注册代理
func registerProxy(conn net.Conn, proxyConfig common.ProxyConfig) error {
	// 发送新建代理请求
	proxyReq := common.NewProxyRequest{
		Name:       proxyConfig.Name,
		RemotePort: proxyConfig.RemotePort,
		ClientID:   clientID,
	}
	reqData, err := json.Marshal(proxyReq)
	if err != nil {
		return fmt.Errorf("序列化代理请求失败: %v", err)
	}

	if err := common.WriteMessage(conn, common.MsgTypeNewProxy, reqData); err != nil {
		return fmt.Errorf("发送代理请求失败: %v", err)
	}

	// 接收新建代理响应
	msg, err := common.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("读取代理响应失败: %v", err)
	}

	if msg.Type != common.MsgTypeNewProxyResp {
		return fmt.Errorf("预期代理响应，收到类型: %d", msg.Type)
	}

	var proxyResp common.NewProxyResponse
	if err := json.Unmarshal(msg.Content, &proxyResp); err != nil {
		return fmt.Errorf("解析代理响应失败: %v", err)
	}

	if !proxyResp.Success {
		return fmt.Errorf("注册代理失败: %s", proxyResp.Error)
	}

	common.Info("代理 %s 注册成功", proxyConfig.Name)
	return nil
}

// 启动本地代理
func startLocalProxy(ctx context.Context, proxyConfig common.ProxyConfig) {
	// 创建工作连接管理器
	connManager := &WorkConnManager{
		minConns:     2,  // 最小连接数
		maxConns:     10, // 最大连接数
		initConns:    5,  // 初始连接数
		proxyConfig:  proxyConfig,
		ctx:          ctx,
		stopCh:       make(chan struct{}),
		activeConns:  0,
		connRequests: 0,
	}

	// 启动工作连接管理器
	connManager.start()

	// 等待停止信号
	select {
	case <-ctx.Done():
		// 主动关闭
	case <-exitCh:
		// 程序退出
	}

	// 停止连接管理器
	connManager.stop()
}

// WorkConnManager 工作连接管理器
type WorkConnManager struct {
	minConns     int
	maxConns     int
	initConns    int
	currentConns int
	activeConns  int32
	connRequests int32
	mu           sync.Mutex
	proxyConfig  common.ProxyConfig
	ctx          context.Context
	stopCh       chan struct{}
	wg           sync.WaitGroup
}

// start 启动连接管理器
func (m *WorkConnManager) start() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 初始化连接池
	for i := 0; i < m.initConns; i++ {
		m.wg.Add(1)
		go m.runWorkConn()
	}

	// 启动动态调整线程
	go m.monitorAndAdjust()
}

// stop 停止连接管理器
func (m *WorkConnManager) stop() {
	close(m.stopCh)
	m.wg.Wait()
}

// runWorkConn 运行单个工作连接处理协程
func (m *WorkConnManager) runWorkConn() {
	defer m.wg.Done()

	retryDelay := 1 * time.Second
	maxRetryDelay := 30 * time.Second

	m.mu.Lock()
	m.currentConns++
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.currentConns--
		m.mu.Unlock()
	}()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-reconnectCh:
			// 控制连接断开，等待重新建立
			time.Sleep(time.Second)
			continue
		default:
			// 记录连接请求
			atomic.AddInt32(&m.connRequests, 1)

			// 创建并管理工作连接
			err := handleWorkConn(m.proxyConfig)

			// 如果连接处理成功，记录活跃连接
			if err == nil {
				atomic.AddInt32(&m.activeConns, 1)
			}

			// 如果出现错误，进行重试
			if err != nil {
				common.Error("工作连接处理错误: %v, 将在 %v 后重试", err, retryDelay)

				// 退避重试
				select {
				case <-time.After(retryDelay):
					// 指数退避增加重试延迟，但不超过最大值
					retryDelay = time.Duration(float64(retryDelay) * 1.5)
					if retryDelay > maxRetryDelay {
						retryDelay = maxRetryDelay
					}
				case <-m.ctx.Done():
					return
				case <-m.stopCh:
					return
				case <-reconnectCh:
					// 如果收到重连信号，重置重试延迟
					retryDelay = 1 * time.Second
				}
			} else {
				// 成功处理了一个连接，重置重试延迟
				retryDelay = 1 * time.Second

				// 减少活跃连接计数
				atomic.AddInt32(&m.activeConns, -1)
			}
		}
	}
}

// monitorAndAdjust 监控并动态调整工作连接数量
func (m *WorkConnManager) monitorAndAdjust() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.adjustConnCount()
		case <-m.ctx.Done():
			return
		case <-m.stopCh:
			return
		}
	}
}

// adjustConnCount 根据负载动态调整连接数量
func (m *WorkConnManager) adjustConnCount() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 获取当前活跃连接数和请求数
	activeConns := atomic.LoadInt32(&m.activeConns)
	requests := atomic.LoadInt32(&m.connRequests)

	// 重置请求计数
	atomic.StoreInt32(&m.connRequests, 0)

	// 计算负载率
	loadFactor := float64(activeConns) / float64(m.currentConns)
	requestRate := float64(requests) / 10.0 // 10秒内的请求率

	// 根据负载动态调整连接数
	if (loadFactor > 0.7 || requestRate > float64(m.currentConns)) && m.currentConns < m.maxConns {
		// 高负载，增加连接
		newCount := m.currentConns + 1
		common.Info("增加工作连接数: %d -> %d (负载: %.2f, 请求率: %.2f)",
			m.currentConns, newCount, loadFactor, requestRate)

		m.wg.Add(1)
		go m.runWorkConn()
	} else if loadFactor < 0.3 && requestRate < float64(m.currentConns)/2 && m.currentConns > m.minConns {
		// 低负载，减少连接
		common.Info("减少工作连接数: %d -> %d (负载: %.2f, 请求率: %.2f)",
			m.currentConns, m.currentConns-1, loadFactor, requestRate)
		// 不需要做什么，等待一个连接自然结束即可
	}
}

// 处理单个工作连接
func handleWorkConn(proxyConfig common.ProxyConfig) error {
	// 创建新工作连接
	workConn, err := createWorkConn(proxyConfig.Name)
	if err != nil {
		return fmt.Errorf("创建工作连接失败: %v", err)
	}
	defer workConn.Close()

	// 获取本地连接
	localConn, err := getLocalConnection(proxyConfig)
	if err != nil {
		return fmt.Errorf("获取本地连接失败: %v", err)
	}
	defer localConn.Close()

	// 转发数据
	common.Info("开始代理数据传输: %s", proxyConfig.Name)
	errCh := common.Transfer(workConn, localConn)

	// 等待数据传输完成或出错
	err = <-errCh
	if err != nil {
		common.Error("数据传输错误: %s, %v", proxyConfig.Name, err)
	}

	return err
}

// 创建工作连接
func createWorkConn(proxyName string) (net.Conn, error) {
	// 连接服务端
	address := fmt.Sprintf("%s:%d", config.ServerAddr, config.ServerPort)

	// 设置连接超时
	dialer := &net.Dialer{
		Timeout: common.DialTimeout,
	}

	conn, err := dialer.Dial("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("连接服务端失败: %v", err)
	}

	// 设置读写超时
	conn.SetDeadline(time.Now().Add(common.HandshakeTimeout))

	// 认证
	if err = authenticate(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("工作连接认证失败: %v", err)
	}

	// 发送新工作连接请求
	workConnReq := common.NewWorkConnRequest{
		ProxyName: proxyName,
		ClientID:  clientID,
	}
	reqData, err := json.Marshal(workConnReq)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("序列化工作连接请求失败: %v", err)
	}

	if err := common.WriteMessage(conn, common.MsgTypeNewWorkConn, reqData); err != nil {
		conn.Close()
		return nil, fmt.Errorf("发送工作连接请求失败: %v", err)
	}

	// 清除读写超时
	conn.SetDeadline(time.Time{})

	return conn, nil
}

// 获取本地连接
func getLocalConnection(proxyConfig common.ProxyConfig) (net.Conn, error) {
	localAddr := fmt.Sprintf("%s:%d", proxyConfig.LocalAddr, proxyConfig.LocalPort)

	// 设置连接超时
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
	}

	conn, err := dialer.Dial("tcp", localAddr)
	if err != nil {
		return nil, fmt.Errorf("连接本地服务失败: %v", err)
	}
	return conn, nil
}
