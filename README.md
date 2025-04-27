# WebSocket 代理服务

这是一个支持 WebSocket/WebSocket Secure (WSS) 的代理服务，可以通过加密通道转发流量。

## 配置 SSL/TLS

### 服务端配置 (server_config.json)

```json
{
    "websocket_host": "0.0.0.0",
    "websocket_port": 8765,
    "use_ssl": true,  // 设置为 true 启用 SSL
    "ssl_cert": "path/to/cert.pem",  // SSL 证书路径
    "ssl_key": "path/to/key.pem"     // SSL 私钥路径
}
```

### 客户端配置 (client_config.json)

```json
{
    "server": "wss://your-server-address:8765",  // 使用 wss:// 前缀连接加密服务器
    "client_id": "client-a1",
    "mappings": [
        {
            "local_ip": "127.0.0.1",
            "local_port": 3000,
            "remote_port": 3001
        }
    ],
    "verify_ssl": true,  // 是否验证服务器证书
    "ca_cert": "path/to/ca.pem",  // CA 证书路径 (可选)
    "client_cert": "path/to/client_cert.pem",  // 客户端证书 (用于双向验证，可选)
    "client_key": "path/to/client_key.pem"     // 客户端密钥 (用于双向验证，可选)
}
```

## 生成 SSL 证书

可以使用 OpenSSL 生成自签名证书，例如：

```bash
# 生成自签名服务器证书
openssl req -x509 -newkey rsa:4096 -keyout .ssl/server_key.pem -out .ssl/server_cert.pem -days 365 -nodes

# 若需要客户端证书 (双向 SSL 验证)
openssl req -x509 -newkey rsa:4096 -keyout .ssl/client_key.pem -out .ssl/client_cert.pem -days 365 -nodes
```

## 启动服务

启动服务器:
```bash
python server.py
```

启动客户端:
```bash
python client.py
```

## 安全提示

- 生产环境中，建议使用由受信任的 CA 签发的证书
- 设置 `verify_ssl: false` 会跳过证书验证，这在生产环境中不安全
- 建议使用 TLS 1.2 或更高版本

使用方式
在公网服务器B上运行 server.py

在内网A上运行 client.py（确保能访问公网IP的 :8765 端口）

外部用户访问 http://B_IP:8080 即可访问内网A上的 localhost:3000