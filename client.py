# client.py
import asyncio
import websockets
import json
import base64
import sys
import ssl
import time
from pathlib import Path

# ======== 加载配置 =========
CONFIG_PATH = "client_config.json"

if not Path(CONFIG_PATH).exists():
    print(f"[错误] 未找到配置文件：{CONFIG_PATH}")
    sys.exit(1)

with open(CONFIG_PATH, "r") as f:
    config = json.load(f)

SERVER = config["server"]
CLIENT_ID = config["client_id"]
MAPPINGS = config["mappings"]
# SSL配置
VERIFY_SSL = config.get("verify_ssl", True)
CA_CERT = config.get("ca_cert")
CLIENT_CERT = config.get("client_cert")
CLIENT_KEY = config.get("client_key")
# 重连配置
RECONNECT_DELAY_MIN = config.get("reconnect_delay_min", 1)  # 初始重连延迟（秒）
RECONNECT_DELAY_MAX = config.get("reconnect_delay_max", 30)  # 最大重连延迟（秒）
RECONNECT_FACTOR = config.get("reconnect_factor", 1.5)  # 重连延迟增长因子

# ===========================

# 创建端口映射表
local_map = {
    m["remote_port"]: (m["local_ip"], m["local_port"]) for m in MAPPINGS
}
# 存储通道连接
channel_map = {}
# 存储通道到远程端口的映射
channel_port_map = {}

# 创建反向映射表，记录每个远程端口对应的映射
remote_port_lookup = {}
for m in MAPPINGS:
    remote_port_lookup[m["remote_port"]] = (m["local_ip"], m["local_port"])

async def handle_client():
    reconnect_delay = RECONNECT_DELAY_MIN
    connection_attempt = 0
    
    while True:
        connection_attempt += 1
        ws = None
        
        try:
            # 配置SSL
            ssl_context = None
            
            # 检查服务器URL是否为WSS
            is_secure = SERVER.startswith("wss://")
            
            if is_secure:
                ssl_context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
                
                if VERIFY_SSL:
                    # 验证服务器证书
                    if CA_CERT:
                        ssl_context.load_verify_locations(CA_CERT)
                        # 如果使用IP地址连接，可以禁用主机名检查
                        if SERVER.split("://")[1].split(":")[0].replace(".", "").isdigit():
                            ssl_context.check_hostname = False
                    else:
                        ssl_context.load_default_certs()
                else:
                    # 不验证服务器证书（不推荐用于生产环境）
                    ssl_context.check_hostname = False
                    ssl_context.verify_mode = ssl.CERT_NONE
                    
                # 如果有客户端证书，用于双向验证
                if CLIENT_CERT and CLIENT_KEY:
                    ssl_context.load_cert_chain(CLIENT_CERT, CLIENT_KEY)
                    
                print(f"[SSL已配置] {'验证' if VERIFY_SSL else '不验证'}服务器证书")
            
            print(f"[连接尝试 #{connection_attempt}] 正在连接到 {SERVER}...")
            async with websockets.connect(SERVER, ssl=ssl_context) as ws:
                # 重置重连延迟
                reconnect_delay = RECONNECT_DELAY_MIN
                print(f"[已连接] {SERVER}")

                # 注册
                await ws.send(json.dumps({
                    "client_id": CLIENT_ID,
                    "mappings": [
                        {"remote_port": m["remote_port"], "local_port": m["local_port"]}
                        for m in MAPPINGS
                    ]
                }))

                # 清空旧通道
                for channel_id in list(channel_map.keys()):
                    reader, writer = channel_map[channel_id]
                    try:
                        writer.close()
                    except:
                        pass
                channel_map.clear()
                channel_port_map.clear()

                # 启动从服务器读取数据的任务
                read_task = asyncio.create_task(read_from_server(ws))
                
                # 保持连接活跃并检测连接状态
                while True:
                    # 检查连接状态
                    try:
                        pong_waiter = await ws.ping()
                        await asyncio.wait_for(pong_waiter, timeout=5)
                    except:
                        print("[连接检测] WebSocket连接似乎已断开")
                        raise websockets.exceptions.ConnectionClosed(
                            None, None, "Connection check failed")
                    
                    await asyncio.sleep(30)  # 每30秒发送一次心跳包

        except websockets.exceptions.InvalidStatusCode as e:
            print(f"[连接失败] 服务器返回错误状态码: {e}")
        except ssl.SSLCertVerificationError as e:
            print(f"[SSL验证失败] {e}")
            print("[建议] 如果使用自签名证书或IP地址连接，可以设置 verify_ssl: false")
        except ssl.SSLError as e:
            print(f"[SSL错误] {e}")
        except ConnectionRefusedError:
            print(f"[连接被拒绝] 服务器可能未运行或端口未开放")
        except websockets.exceptions.ConnectionClosed as e:
            print(f"[连接断开] {e}")
        except Exception as e:
            print(f"[连接失败，错误类型：{type(e).__name__}] {e}")
        finally:
            # 清理通道连接
            for channel_id in list(channel_map.keys()):
                try:
                    reader, writer = channel_map.pop(channel_id)
                    writer.close()
                except:
                    pass
            
            # 计算下一次重连延迟
            wait_time = min(reconnect_delay, RECONNECT_DELAY_MAX)
            reconnect_delay = min(reconnect_delay * RECONNECT_FACTOR, RECONNECT_DELAY_MAX)
            
            print(f"[重连] 将在 {wait_time:.1f} 秒后重试...")
            await asyncio.sleep(wait_time)

async def read_from_server(ws):
    async for msg in ws:
        try:
            data = json.loads(msg)
            channel_id = data["channel"]
            type_ = data["type"]

            if type_ == "connect":
                # 处理新连接建立
                remote_port = data["remote_port"]
                channel_port_map[channel_id] = remote_port
                
                try:
                    local_ip, local_port = local_map[remote_port]
                except KeyError:
                    print(f"[错误] 找不到远程端口 {remote_port} 的映射配置")
                    continue
                
                try:
                    print(f"[收到连接] 远程端口 {remote_port} -> 本地 {local_ip}:{local_port}")
                    reader, writer = await asyncio.open_connection(local_ip, local_port)
                    
                    # 先检查是否有旧连接
                    if channel_id in channel_map:
                        old_reader, old_writer = channel_map[channel_id]
                        try:
                            old_writer.close()
                        except:
                            pass
                            
                    channel_map[channel_id] = (reader, writer)
                    
                    # 启动从本地到WebSocket的转发任务
                    asyncio.create_task(forward_local_to_ws(reader, ws, channel_id))
                    print(f"[已建立] 通道 {channel_id} 连接到 {local_ip}:{local_port}")
                except ConnectionRefusedError:
                    print(f"[连接失败] 本地服务 {local_ip}:{local_port} 拒绝连接")
                except Exception as e:
                    print(f"[连接失败] 无法连接到 {local_ip}:{local_port}: {e}")

            elif type_ == "data":
                payload = base64.b64decode(data["payload"])

                if channel_id not in channel_map:
                    if channel_id in channel_port_map:
                        # 已知远程端口，但连接断开，尝试重连
                        remote_port = channel_port_map[channel_id]
                        
                        try:
                            local_ip, local_port = local_map[remote_port]
                        except KeyError:
                            print(f"[错误] 找不到远程端口 {remote_port} 的映射配置")
                            continue
                            
                        try:
                            print(f"[重新连接] 远程端口 {remote_port} -> 本地 {local_ip}:{local_port}")
                            reader, writer = await asyncio.open_connection(local_ip, local_port)
                            channel_map[channel_id] = (reader, writer)
                            
                            # 启动从本地到WebSocket的转发任务
                            asyncio.create_task(forward_local_to_ws(reader, ws, channel_id))
                        except Exception as e:
                            print(f"[重连失败] 无法连接到 {local_ip}:{local_port}: {e}")
                            continue
                    else:
                        print(f"[错误] 收到未知通道 {channel_id} 的数据")
                        continue

                try:
                    reader, writer = channel_map[channel_id]
                    writer.write(payload)
                    await writer.drain()
                except ConnectionResetError:
                    print(f"[错误] 通道 {channel_id} 连接已重置")
                    channel_map.pop(channel_id, None)
                except Exception as e:
                    print(f"[错误] 通道 {channel_id} 写入失败: {e}")
                    channel_map.pop(channel_id, None)

            elif type_ == "close":
                if channel_id in channel_map:
                    reader, writer = channel_map.pop(channel_id)
                    try:
                        writer.close()
                    except:
                        pass
                    print(f"[关闭] 通道 {channel_id}")
                    
        except json.JSONDecodeError:
            print(f"[错误] 无效的JSON消息: {msg[:100]}...")
        except KeyError as e:
            print(f"[错误] 消息缺少必要字段 {e}: {msg[:100]}...")
        except Exception as e:
            print(f"[错误] 处理消息失败: {e}")

async def forward_local_to_ws(reader, ws, channel_id):
    try:
        while not reader.at_eof():
            try:
                data = await reader.read(4096)
                if not data:
                    break
                
                # 检查WebSocket状态的另一种方式
                try:
                    await ws.send(json.dumps({
                        "channel": channel_id,
                        "type": "data",
                        "payload": base64.b64encode(data).decode()
                    }))
                except websockets.exceptions.ConnectionClosed:
                    print(f"[错误] 通道 {channel_id} WebSocket已关闭")
                    break
            except ConnectionResetError:
                print(f"[错误] 通道 {channel_id} 本地连接已重置")
                break
            except Exception as e:
                print(f"[错误] 通道 {channel_id} 转发失败: {e}")
                break
    finally:
        # 关闭通道
        if channel_id in channel_map:
            reader, writer = channel_map.pop(channel_id)
            try:
                writer.close()
            except:
                pass
                
        # 通知服务器关闭通道
        try:
            # 使用异常捕获来检查连接状态
            await ws.send(json.dumps({
                "channel": channel_id,
                "type": "close"
            }))
        except websockets.exceptions.ConnectionClosed:
            # WebSocket已关闭，无需发送
            pass
        except Exception as e:
            print(f"[错误] 发送关闭通知失败: {e}")
                
        print(f"[关闭] 通道 {channel_id}")

asyncio.run(handle_client())
