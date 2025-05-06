package common

import (
	"encoding/json"
	"os"
)

// ServerConfig 服务端配置
type ServerConfig struct {
	BindAddr string `json:"bind_addr"` // 服务端绑定地址
	BindPort int    `json:"bind_port"` // 服务端绑定端口
	Token    string `json:"token"`     // 认证令牌
}

// ClientConfig 客户端配置
type ClientConfig struct {
	ServerAddr string        `json:"server_addr"` // 服务端地址
	ServerPort int           `json:"server_port"` // 服务端端口
	Token      string        `json:"token"`       // 认证令牌
	Proxies    []ProxyConfig `json:"proxies"`     // 代理配置列表
}

// ProxyConfig 代理配置
type ProxyConfig struct {
	Name       string `json:"name"`        // 代理名称
	LocalAddr  string `json:"local_addr"`  // 本地地址
	LocalPort  int    `json:"local_port"`  // 本地端口
	RemotePort int    `json:"remote_port"` // 远程端口
}

// LoadServerConfig 加载服务端配置
func LoadServerConfig(path string) (*ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config ServerConfig
	if err = json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return &config, nil
}

// LoadClientConfig 加载客户端配置
func LoadClientConfig(path string) (*ClientConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config ClientConfig
	if err = json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return &config, nil
}
