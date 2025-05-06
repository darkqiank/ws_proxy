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
	config *common.ClientConfig

	// 工作连接管理
	controlConn net.Conn
	workConnCh  = make(map[string]chan net.Conn)
	workConnMu  sync.Mutex
)

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

	// 初始化通道
	for _, proxyConfig := range config.Proxies {
		workConnCh[proxyConfig.Name] = make(chan net.Conn, 10)
	}

	// 启动客户端
	for {
		if err := runClient(); err != nil {
			common.Error("客户端运行出错: %v", err)
			time.Sleep(5 * time.Second)
			common.Info("尝试重连...")
			continue
		}
		break
	}
}

// 运行客户端
func runClient() error {
	var err error

	// 连接服务端
	address := fmt.Sprintf("%s:%d", config.ServerAddr, config.ServerPort)
	controlConn, err = net.Dial("tcp", address)
	if err != nil {
		return fmt.Errorf("连接服务端失败: %v", err)
	}
	defer controlConn.Close()

	common.Info("已连接到服务端: %s", address)

	// 客户端认证
	if err = authenticate(); err != nil {
		return fmt.Errorf("认证失败: %v", err)
	}

	// 注册所有代理
	for _, proxyConfig := range config.Proxies {
		if err = registerProxy(proxyConfig); err != nil {
			return fmt.Errorf("注册代理 %s 失败: %v", proxyConfig.Name, err)
		}
	}

	// 启动工作连接处理
	var wg sync.WaitGroup
	for _, proxyConfig := range config.Proxies {
		wg.Add(1)
		go startLocalProxy(proxyConfig, &wg)
	}

	// 等待所有代理结束
	wg.Wait()
	return nil
}

// 客户端认证
func authenticate() error {
	// 发送认证请求
	authReq := common.AuthRequest{
		Token: config.Token,
	}
	reqData, err := json.Marshal(authReq)
	if err != nil {
		return fmt.Errorf("序列化认证请求失败: %v", err)
	}

	if err := common.WriteMessage(controlConn, common.MsgTypeAuth, reqData); err != nil {
		return fmt.Errorf("发送认证请求失败: %v", err)
	}

	// 接收认证响应
	msg, err := common.ReadMessage(controlConn)
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
func registerProxy(proxyConfig common.ProxyConfig) error {
	// 发送新建代理请求
	proxyReq := common.NewProxyRequest{
		Name:       proxyConfig.Name,
		RemotePort: proxyConfig.RemotePort,
	}
	reqData, err := json.Marshal(proxyReq)
	if err != nil {
		return fmt.Errorf("序列化代理请求失败: %v", err)
	}

	if err := common.WriteMessage(controlConn, common.MsgTypeNewProxy, reqData); err != nil {
		return fmt.Errorf("发送代理请求失败: %v", err)
	}

	// 接收新建代理响应
	msg, err := common.ReadMessage(controlConn)
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
func startLocalProxy(proxyConfig common.ProxyConfig, wg *sync.WaitGroup) {
	defer wg.Done()

	// 启动多个工作连接
	const workConnCount = 5
	var workWg sync.WaitGroup
	for i := 0; i < workConnCount; i++ {
		workWg.Add(1)
		go func() {
			defer workWg.Done()
			for {
				// 创建新工作连接
				workConn, err := createWorkConn(proxyConfig.Name)
				if err != nil {
					common.Error("创建工作连接失败: %v", err)
					time.Sleep(5 * time.Second)
					continue
				}

				// 获取本地连接
				localConn, err := getLocalConnection(proxyConfig)
				if err != nil {
					common.Error("获取本地连接失败: %v", err)
					workConn.Close()
					continue
				}

				// 转发数据
				common.Info("开始代理数据传输: %s", proxyConfig.Name)
				common.Transfer(workConn, localConn)
			}
		}()
	}

	workWg.Wait()
}

// 创建工作连接
func createWorkConn(proxyName string) (net.Conn, error) {
	// 连接服务端
	address := fmt.Sprintf("%s:%d", config.ServerAddr, config.ServerPort)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("连接服务端失败: %v", err)
	}

	// 认证
	if err = doAuthForConn(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("工作连接认证失败: %v", err)
	}

	// 发送新工作连接请求
	workConnReq := common.NewWorkConnRequest{
		ProxyName: proxyName,
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

	return conn, nil
}

// 为工作连接进行认证
func doAuthForConn(conn net.Conn) error {
	// 发送认证请求
	authReq := common.AuthRequest{
		Token: config.Token,
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

	return nil
}

// 获取本地连接
func getLocalConnection(proxyConfig common.ProxyConfig) (net.Conn, error) {
	localAddr := fmt.Sprintf("%s:%d", proxyConfig.LocalAddr, proxyConfig.LocalPort)
	conn, err := net.Dial("tcp", localAddr)
	if err != nil {
		return nil, fmt.Errorf("连接本地服务失败: %v", err)
	}
	return conn, nil
}
