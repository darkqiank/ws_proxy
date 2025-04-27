# client.py
import asyncio
import websockets
import json
import base64
import sys
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
    while True:
        try:
            async with websockets.connect(SERVER) as ws:
                print(f"[已连接] {SERVER}")

                # 注册
                await ws.send(json.dumps({
                    "client_id": CLIENT_ID,
                    "mappings": [
                        {"remote_port": m["remote_port"], "local_port": m["local_port"]}
                        for m in MAPPINGS
                    ]
                }))

                asyncio.create_task(read_from_server(ws))

                while True:
                    await asyncio.sleep(1)

        except Exception as e:
            print(f"[连接失败，重试中] {e}")
            await asyncio.sleep(3)

async def read_from_server(ws):
    async for msg in ws:
        data = json.loads(msg)
        channel_id = data["channel"]
        type_ = data["type"]

        if type_ == "connect":
            # 处理新连接建立
            remote_port = data["remote_port"]
            channel_port_map[channel_id] = remote_port
            local_ip, local_port = local_map[remote_port]
            
            try:
                print(f"[收到连接] 远程端口 {remote_port} -> 本地 {local_ip}:{local_port}")
                reader, writer = await asyncio.open_connection(local_ip, local_port)
                channel_map[channel_id] = (reader, writer)
                
                # 启动从本地到WebSocket的转发任务
                asyncio.create_task(forward_local_to_ws(reader, ws, channel_id))
                print(f"[已建立] 通道 {channel_id} 连接到 {local_ip}:{local_port}")
            except Exception as e:
                print(f"[连接失败] 无法连接到 {local_ip}:{local_port}: {e}")

        elif type_ == "data":
            payload = base64.b64decode(data["payload"])

            if channel_id not in channel_map:
                if channel_id in channel_port_map:
                    # 已知远程端口，但连接断开，尝试重连
                    remote_port = channel_port_map[channel_id]
                    local_ip, local_port = local_map[remote_port]
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

            reader, writer = channel_map[channel_id]
            writer.write(payload)
            await writer.drain()

        elif type_ == "close":
            if channel_id in channel_map:
                reader, writer = channel_map[channel_id]
                writer.close()
                del channel_map[channel_id]
                print(f"[关闭] 通道 {channel_id}")

async def forward_local_to_ws(reader, ws, channel_id):
    try:
        while not reader.at_eof():
            data = await reader.read(4096)
            if not data:
                break
            await ws.send(json.dumps({
                "channel": channel_id,
                "type": "data",
                "payload": base64.b64encode(data).decode()
            }))
    finally:
        await ws.send(json.dumps({
            "channel": channel_id,
            "type": "close"
        }))
        if channel_id in channel_map:
            reader, writer = channel_map[channel_id]
            writer.close()
            del channel_map[channel_id]
            print(f"[关闭] 通道 {channel_id}")

asyncio.run(handle_client())
