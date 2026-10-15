import asyncio
import json
import time
import websockets
import requests

async def main():
    print("[*] 正在监听 Excel Copilot 内部会话与网络请求...")
    last_count = 0
    while True:
        try:
            res = requests.get('http://127.0.0.1:9222/json', timeout=1).json()
            target = next((t for t in res if 'Excel Copilot Chat Add-in' in t.get('title', '')), None)
            if target:
                ws_url = target['webSocketDebuggerUrl']
                async with websockets.connect(ws_url, max_size=10*1024*1024) as ws:
                    cmd = {'id': 1, 'method': 'Runtime.evaluate', 'params': {'expression': 'window.__captured_requests__ || []', 'returnByValue': True}}
                    await ws.send(json.dumps(cmd))
                    resp = await ws.recv()
                    val = json.loads(resp)['result']['result']['value']
                    if len(val) > last_count:
                        print(f"\n[!] 捕获到 {len(val) - last_count} 个新请求！")
                        for item in val[last_count:]:
                            print(f" -> {item.get('time')} {item.get('method')} {item.get('url')}")
                            if item.get('body'):
                                print(f"    Body: {str(item.get('body'))[:300]}...")
                        last_count = len(val)
                        with open("tools/office_copilot_request.json", "w", encoding="utf-8") as f:
                            json.dump(val, f, ensure_ascii=False, indent=2)
        except Exception as e:
            pass
        await asyncio.sleep(1)

if __name__ == '__main__':
    asyncio.run(main())
