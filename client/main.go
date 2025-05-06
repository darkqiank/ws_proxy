package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
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
	controlConn, err = net.Dial("tcp", address)
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

	// 保持连接活跃，检测断开
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// 简单的心跳检测，可扩展为实际的心跳消息
			if _, err := controlConn.Write([]byte{0}); err != nil {
				return fmt.Errorf("心跳检测失败: %v", err)
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
	// 启动多个工作连接
	const workConnCount = 5
	var workWg sync.WaitGroup
	var stopCh = make(chan struct{})

	// 启动工作连接管理器
	for i := 0; i < workConnCount; i++ {
		workWg.Add(1)
		go func() {
			defer workWg.Done()
			startWorkConn(ctx, stopCh, proxyConfig)
		}()
	}

	// 等待停止信号
	select {
	case <-ctx.Done():
		// 主动关闭
	case <-exitCh:
		// 程序退出
	}

	// 通知所有工作连接停止
	close(stopCh)
	// 等待所有工作连接退出
	workWg.Wait()
}

// 启动工作连接
func startWorkConn(ctx context.Context, stopCh chan struct{}, proxyConfig common.ProxyConfig) {
	retryDelay := 1 * time.Second
	maxRetryDelay := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case <-reconnectCh:
			// 控制连接断开，需要等待重新建立
			time.Sleep(time.Second)
			continue
		default:
			// 创建并管理工作连接
			if err := handleWorkConn(proxyConfig); err != nil {
				common.Error("工作连接处理错误: %v, 将在 %v 后重试", err, retryDelay)

				// 退避重试
				select {
				case <-time.After(retryDelay):
					// 指数退避增加重试延迟，但不超过最大值
					retryDelay = time.Duration(float64(retryDelay) * 1.5)
					if retryDelay > maxRetryDelay {
						retryDelay = maxRetryDelay
					}
				case <-ctx.Done():
					return
				case <-stopCh:
					return
				case <-reconnectCh:
					// 如果收到重连信号，重置重试延迟
					retryDelay = 1 * time.Second
				}
			} else {
				// 成功处理了一个连接，重置重试延迟
				retryDelay = 1 * time.Second
			}
		}
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
	common.Transfer(workConn, localConn)
	return nil
}

// 创建工作连接
func createWorkConn(proxyName string) (net.Conn, error) {
	// 连接服务端
	address := fmt.Sprintf("%s:%d", config.ServerAddr, config.ServerPort)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("连接服务端失败: %v", err)
	}

	// 设置读写超时
	conn.SetDeadline(time.Now().Add(10 * time.Second))

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
