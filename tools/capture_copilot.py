import asyncio
import json
import os
import sys
import time
import requests
import websockets

CDP_HTTP_URL = "http://127.0.0.1:9222/json"
OUTPUT_FILE = "tools/copilot_traffic_dump.json"

captured_data = []

async def listen_target(target):
    ws_url = target.get("webSocketDebuggerUrl")
    title = target.get("title", "")
    url = target.get("url", "")
    print(f"\n[+] 正在附加到目标页面: {title} ({url[:80]}...)")

    try:
        async with websockets.connect(ws_url, max_size=10*1024*1024) as ws:
            # 开启网络监听
            await ws.send(json.dumps({"id": 1, "method": "Network.enable", "params": {}}))
            req_map = {}

            while True:
                msg = await ws.recv()
                event = json.loads(msg)
                method = event.get("method")
                params = event.get("params", {})

                if method == "Network.requestWillBeSent":
                    req_id = params.get("requestId")
                    req = params.get("request", {})
                    req_url = req.get("url", "")

                    # 过滤只关注 Copilot / Substrate / Office 相关请求
                    interest = any(k in req_url.lower() for k in [
                        "substrate", "copilot", "officeapps.live.com", "openai",
                        "api.cortana.ai", "sydney", "bing.com/chat", "bing.com/sydney"
                    ])

                    if interest:
                        print(f"\n[>>> 捕获请求] {req.get('method')} {req_url}")
                        auth = req.get("headers", {}).get("Authorization") or req.get("headers", {}).get("authorization")
                        if auth:
                            print(f"    [Auth Token 发现]: {auth[:35]}... (总长: {len(auth)})")

                        post_data = req.get("postData")
                        if post_data:
                            print(f"    [Post Data 摘要]: {post_data[:200]}...")

                        record = {
                            "timestamp": time.time(),
                            "type": "request",
                            "requestId": req_id,
                            "method": req.get("method"),
                            "url": req_url,
                            "headers": req.get("headers"),
                            "postData": post_data
                        }
                        captured_data.append(record)
                        req_map[req_id] = record

                        with open(OUTPUT_FILE, "w", encoding="utf-8") as f:
                            json.dump(captured_data, f, ensure_ascii=False, indent=2)

                elif method == "Network.responseReceived":
                    req_id = params.get("requestId")
                    if req_id in req_map:
                        res = params.get("response", {})
                        status = res.get("status")
                        mime = res.get("mimeType")
                        print(f"[<<< 收到响应] Status: {status}, MimeType: {mime}")

                elif method == "Network.loadingFinished":
                    req_id = params.get("requestId")
                    if req_id in req_map:
                        # 尝试获取响应体
                        call_id = int(time.time() * 1000) % 1000000 + 100
                        await ws.send(json.dumps({
                            "id": call_id,
                            "method": "Network.getResponseBody",
                            "params": {"requestId": req_id}
                        }))

                elif "id" in event and event["id"] > 100:
                    result = event.get("result", {})
                    body = result.get("body")
                    if body:
                        print(f"    [响应内容 长度]: {len(body)} 字节")
                        captured_data.append({
                            "type": "responseBody",
                            "body": body[:5000]
                        })
                        with open(OUTPUT_FILE, "w", encoding="utf-8") as f:
                            json.dump(captured_data, f, ensure_ascii=False, indent=2)

    except Exception as e:
        print(f"[-] 目标页面连接断开或发生异常: {e}")

async def main():
    print(f"[*] 启动 Copilot 流量监听服务...")
    print(f"[*] 正在等待 Office / WebView2 在 127.0.0.1:9222 开启...")
    connected_targets = set()

    while True:
        try:
            resp = requests.get(CDP_HTTP_URL, timeout=1)
            targets = resp.json()
            for t in targets:
                t_id = t.get("id")
                t_url = t.get("url", "")
                if t_id not in connected_targets:
                    connected_targets.add(t_id)
                    asyncio.create_task(listen_target(t))
        except Exception:
            pass
        await asyncio.sleep(1)

if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        print("\n[*] 退出监听。")
