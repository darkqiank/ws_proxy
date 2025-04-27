# server.py
import asyncio
import websockets
import uuid
import json
import base64

clients = set()
channels = {}

async def ws_handler(websocket):
    print("客户端A已连接")
    clients.add(websocket)

    try:
        async for msg in websocket:
            try:
                data = json.loads(msg)
                channel_id = data["channel"]
                type_ = data["type"]

                if type_ == "data":
                    payload = base64.b64decode(data["payload"])
                    if channel_id in channels:
                        channels[channel_id].write(payload)
                        await channels[channel_id].drain()
                elif type_ == "close":
                    if channel_id in channels:
                        channels[channel_id].close()
                        del channels[channel_id]
            except Exception as e:
                print(f"处理消息失败：{e}")

    finally:
        clients.remove(websocket)

async def tcp_handler(reader, writer):
    if not clients:
        print("没有可用的A端连接")
        writer.close()
        return

    websocket = list(clients)[0]
    channel_id = str(uuid.uuid4())
    channels[channel_id] = writer

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
        except Exception as e:
            print(f"转发失败: {e}")
        finally:
            await websocket.send(json.dumps({
                "channel": channel_id,
                "type": "close"
            }))

    await tcp_to_ws()
    if channel_id in channels:
        del channels[channel_id]
    writer.close()

async def main():
    ws_server = await websockets.serve(ws_handler, "0.0.0.0", 8765)
    tcp_server = await asyncio.start_server(tcp_handler, "0.0.0.0", 8080)
    print("服务运行中：WS端口8765，对外转发端口8080")
    async with ws_server, tcp_server:
        await asyncio.gather(ws_server.wait_closed(), tcp_server.serve_forever())

asyncio.run(main())
