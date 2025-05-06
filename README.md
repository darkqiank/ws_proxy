# GoTunnel

一个用Go实现的简单内网穿透工具，类似于frp。

## 功能

- 服务端部署在公网服务器上
- 客户端部署在内网服务器上
- 支持TCP端口转发
- 简单的认证机制

## 使用方法

### 服务端

```bash
cd server
go build -o server
./server -config config.json
```

### 客户端

```bash
cd client
go build -o client
./client -config config.json
```

## 配置文件示例

### 服务端配置 (server/config.json)

```json
{
  "bind_addr": "0.0.0.0",
  "bind_port": 7000,
  "token": "your_secure_token"
}
```

### 客户端配置 (client/config.json)

```json
{
  "server_addr": "your_server_public_ip",
  "server_port": 7000,
  "token": "your_secure_token",
  "proxies": [
    {
      "name": "web",
      "local_addr": "127.0.0.1",
      "local_port": 80,
      "remote_port": 8080
    },
    {
      "name": "ssh",
      "local_addr": "127.0.0.1",
      "local_port": 22,
      "remote_port": 2222
    }
  ]
}
``` 