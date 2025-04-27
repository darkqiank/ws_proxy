# server.py
import asyncio
import websockets
import json
import base64
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
# =================================

client_connections = {}  # client_id => websocket
port_client_map = {}     # remote_port => client_id
channels = {}            # channel_id => (writer, websocket)

async def register_client(websocket):
    try:
        msg = await websocket.recv()
        reg = json.loads(msg)
        client_id = reg["client_id"]
        mappings = reg["mappings"]

        client_connections[client_id] = websocket
        for m in mappings:
            port_client_map[m["remote_port"]] = client_id

            async def handler(reader, writer, remote_port=m["remote_port"]):
                await handle_tcp_connection(remote_port, reader, writer)

            asyncio.create_task(asyncio.start_server(handler, WS_HOST, m["remote_port"]))
            print(f"[注册] client_id={client_id}, 映射公网端口 {m['remote_port']} -> 本地端口 {m['local_port']}")

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
                    writer, _ = channels[channel_id]
                    writer.write(payload)
                    await writer.drain()

            elif type_ == "close":
                if channel_id in channels:
                    writer, _ = channels[channel_id]
                    writer.close()
                    del channels[channel_id]

    except Exception as e:
        print(f"[{client_id}] 通信错误: {e}")
    finally:
        print(f"[{client_id}] 连接断开")
        client_connections.pop(client_id, None)

async def handle_tcp_connection(remote_port, reader, writer):
    client_id = port_client_map.get(remote_port)
    websocket = client_connections.get(client_id)
    if not websocket:
        print(f"[错误] 没有客户端可用 for 端口 {remote_port}")
        writer.close()
        return

    channel_id = f"{client_id}:{id(writer)}"
    channels[channel_id] = (writer, websocket)

    async def tcp_to_ws():
        try:
            while not reader.at_eof():
                data = await reader.read(4096)
                if not data:
                    break
                await websocket.send(json.dumps({
                    "channel": channel_id,
                    "type": "data",
                    "payload": base64.b64encode(data).decode()
                }))
        except:
            pass
        finally:
            await websocket.send(json.dumps({
                "channel": channel_id,
                "type": "close"
            }))
            channels.pop(channel_id, None)
            writer.close()

    asyncio.create_task(tcp_to_ws())

async def main():
    print(f"[启动服务] WebSocket监听 {WS_HOST}:{WS_PORT}")
    async with websockets.serve(register_client, WS_HOST, WS_PORT):
        await asyncio.Future()  # run forever

asyncio.run(main())
