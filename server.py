# server.py
import asyncio
import websockets
import json
import base64
import ssl
from collections import defaultdict
from pathlib import Path

# ======== 加载配置文件 =========
CONFIG_PATH = "server_config.json"

if not Path(CONFIG_PATH).exists():
    print(f"[错误] 未找到配置文件：{CONFIG_PATH}")
    exit(1)

with open(CONFIG_PATH, "r") as f:
    config = json.load(f)

WS_HOST = config.get("websocket_host", "0.0.0.0")
WS_PORT = config.get("websocket_port", 8765)
USE_SSL = config.get("use_ssl", False)
SSL_CERT = config.get("ssl_cert")
SSL_KEY = config.get("ssl_key")
# =================================

client_connections = {}  # client_id => websocket
port_client_map = {}     # remote_port => client_id
channels = {}            # channel_id => (writer, websocket, remote_port)
active_servers = {}      # remote_port => server

async def register_client(websocket):
    try:
        msg = await websocket.recv()
        reg = json.loads(msg)
        client_id = reg["client_id"]
        mappings = reg["mappings"]
        
        # 如果客户端已存在，先清理旧连接
        if client_id in client_connections:
            old_websocket = client_connections[client_id]
            try:
                await old_websocket.close()
            except:
                pass
            
        client_connections[client_id] = websocket
        
        for m in mappings:
            remote_port = m["remote_port"]
            port_client_map[remote_port] = client_id

            # 检查是否已有此端口的服务器
            if remote_port in active_servers:
                # 如果是同一客户端的重连，无需创建新服务器
                continue
                
            async def handler(reader, writer, remote_port=remote_port):
                await handle_tcp_connection(remote_port, reader, writer)

            try:
                server = await asyncio.start_server(handler, WS_HOST, remote_port)
                active_servers[remote_port] = server
                print(f"[注册] client_id={client_id}, 映射公网端口 {remote_port} -> 本地端口 {m['local_port']}")
            except OSError as e:
                if e.errno == 48:  # 地址已被使用
                    print(f"[警告] 端口 {remote_port} 已被占用，尝试关闭旧连接")
                    try:
                        # 关闭旧服务器
                        if remote_port in active_servers:
                            old_server = active_servers[remote_port]
                            old_server.close()
                            await old_server.wait_closed()
                            del active_servers[remote_port]
                            
                            # 重新尝试启动服务器
                            server = await asyncio.start_server(handler, WS_HOST, remote_port)
                            active_servers[remote_port] = server
                            print(f"[注册] client_id={client_id}, 映射公网端口 {remote_port} -> 本地端口 {m['local_port']}")
                    except Exception as inner_e:
                        print(f"[错误] 无法重用端口 {remote_port}: {inner_e}")
                else:
                    print(f"[错误] 创建服务器失败 on 端口 {remote_port}: {e}")

        await listen_from_client(websocket, client_id)

    except Exception as e:
        print(f"[注册失败] {e}")

async def listen_from_client(websocket, client_id):
    try:
        async for msg in websocket:
            data = json.loads(msg)
            channel_id = data["channel"]
            type_ = data["type"]

            if type_ == "data":
                payload = base64.b64decode(data["payload"])
                if channel_id in channels:
                    writer, _, _ = channels[channel_id]
                    writer.write(payload)
                    await writer.drain()

            elif type_ == "close":
                if channel_id in channels:
                    writer, _, _ = channels[channel_id]
                    writer.close()
                    channels.pop(channel_id, None)

    except websockets.exceptions.ConnectionClosed:
        print(f"[{client_id}] 连接断开")
    except Exception as e:
        print(f"[{client_id}] 通信错误: {e}")
    finally:
        # 清理资源
        print(f"[{client_id}] 连接断开，清理资源")
        # 从连接表中移除
        client_connections.pop(client_id, None)
        
        # 移除该客户端的所有通道
        channels_to_remove = [cid for cid in channels if cid.startswith(f"{client_id}:")]
        for cid in channels_to_remove:
            try:
                writer, _, _ = channels[cid]
                writer.close()
            except:
                pass
            channels.pop(cid, None)
            
        # 注意：不要关闭端口服务器，让它继续监听，等待客户端重连

async def handle_tcp_connection(remote_port, reader, writer):
    client_id = port_client_map.get(remote_port)
    websocket = client_connections.get(client_id)
    if not websocket:
        print(f"[错误] 没有客户端可用 for 端口 {remote_port}")
        writer.close()
        return

    channel_id = f"{client_id}:{id(writer)}"
    channels[channel_id] = (writer, websocket, remote_port)
    print(f"[建立连接] 通道 {channel_id} 端口 {remote_port}")

    # 发送连接建立通知给客户端，包含远程端口信息
    try:
        await websocket.send(json.dumps({
            "channel": channel_id,
            "type": "connect",
            "remote_port": remote_port
        }))
    except websockets.exceptions.ConnectionClosed:
        print(f"[错误] 发送连接通知时客户端已断开")
        writer.close()
        return

    async def tcp_to_ws():
        try:
            while not reader.at_eof():
                data = await reader.read(4096)
                if not data:
                    break
                try:
                    await websocket.send(json.dumps({
                        "channel": channel_id,
                        "type": "data",
                        "payload": base64.b64encode(data).decode()
                    }))
                except websockets.exceptions.ConnectionClosed:
                    print(f"[通道 {channel_id}] 发送数据时检测到WebSocket断开")
                    break
        except Exception as e:
            print(f"[通道 {channel_id}] 转发错误: {e}")
        finally:
            try:
                if not websocket.closed:
                    await websocket.send(json.dumps({
                        "channel": channel_id,
                        "type": "close"
                    }))
            except:
                pass
                
            if channel_id in channels:
                channels.pop(channel_id, None)
                
            writer.close()
            print(f"[关闭连接] 通道 {channel_id}")

    asyncio.create_task(tcp_to_ws())

async def main():
    # 配置SSL (若启用)
    ssl_context = None
    protocol = "ws"
    
    if USE_SSL and SSL_CERT and SSL_KEY:
        ssl_context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        try:
            ssl_context.load_cert_chain(SSL_CERT, SSL_KEY)
            protocol = "wss"
            print(f"[SSL已启用] 已加载证书: {SSL_CERT}")
        except Exception as e:
            print(f"[SSL加载失败] {e}")
            ssl_context = None
            
    print(f"[启动服务] {protocol}://{WS_HOST}:{WS_PORT}")
    async with websockets.serve(register_client, WS_HOST, WS_PORT, ssl=ssl_context):
        await asyncio.Future()  # run forever

asyncio.run(main())
