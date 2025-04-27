# client.py
import asyncio
import websockets
import json
import base64

REMOTE_WS = "ws://127.0.0.1:8765"
LOCAL_HOST = "127.0.0.1"
LOCAL_PORT = 3000

channel_map = {}

async def handle_ws():
    while True:
        try:
            async with websockets.connect(REMOTE_WS) as websocket:
                print("已连接到公网服务器")

                async def read_ws():
                    async for msg in websocket:
                        data = json.loads(msg)
                        channel = data["channel"]
                        type_ = data["type"]

                        if type_ == "data":
                            payload = base64.b64decode(data["payload"])
                            if channel not in channel_map:
                                reader, writer = await asyncio.open_connection(LOCAL_HOST, LOCAL_PORT)
                                channel_map[channel] = (reader, writer)

                                asyncio.create_task(forward_local_to_ws(reader, channel, websocket))

                            _, writer = channel_map[channel]
                            writer.write(payload)
                            await writer.drain()

                        elif type_ == "close":
                            if channel in channel_map:
                                _, writer = channel_map[channel]
                                writer.close()
                                del channel_map[channel]

                await read_ws()

        except Exception as e:
            print(f"连接失败: {e}")
            await asyncio.sleep(3)

async def forward_local_to_ws(reader, channel, websocket):
    try:
        while not reader.at_eof():
            data = await reader.read(4096)
            if not data:
                break
            await websocket.send(json.dumps({
                "channel": channel,
                "type": "data",
                "payload": base64.b64encode(data).decode()
            }))
    finally:
        await websocket.send(json.dumps({
            "channel": channel,
            "type": "close"
        }))
        if channel in channel_map:
            _, writer = channel_map[channel]
            writer.close()
            del channel_map[channel]

asyncio.run(handle_ws())
