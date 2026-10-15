"""Opt-in Word Opus image protocol probe using a synthetic, random image.

Never prints credentials, upload URLs, upstream raw frames, or private content.
Does not start project services or execute Office document tools.
"""
import argparse
import base64
import io
import json
from pathlib import Path
import secrets
import sys
import threading
import time
import urllib.parse
import uuid

import requests
import websocket
from PIL import Image, ImageDraw, ImageFont

from office_copilot_reasoning_probe import credentials


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-live", action="store_true")
    parser.add_argument("--proxy", default="http://127.0.0.1:7897")
    args = parser.parse_args()
    if not args.run_live:
        parser.error("--run-live required: uploads a synthetic image and sends inference")
    cred = credentials(Path(__file__).resolve().parents[1])
    expected = secrets.token_hex(4).upper()
    img = Image.new("RGB", (640, 180), "white")
    ImageDraw.Draw(img).text((35, 45), expected, font=ImageFont.truetype("C:/Windows/Fonts/arial.ttf", 68), fill="black")
    buf = io.BytesIO()
    img.save(buf, format="PNG")
    image_data = "data:image/png;base64," + base64.b64encode(buf.getvalue()).decode()
    conv = str(uuid.uuid4())
    result = {"reference": "ImageFile"}
    ws = None
    stop = threading.Event()
    lock = threading.Lock()

    def send(frame):
        with lock:
            ws.send(json.dumps(frame, separators=(",", ":")) + "\x1e")

    def ping():
        while not stop.wait(15):
            try:
                send({"type": 6})
            except Exception:
                return

    try:
        response = requests.post(
            "https://substrate.office.com/m365Copilot/UploadFile",
            headers={"Authorization": "Bearer " + cred["access_token"],
                     "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36 Edg/154.0.0.0",
                     "Origin": "https://fa000000125.resources.office.net",
                     "X-Scenario": cred.get("scenario", ""),
                     "X-Variants": "feature.EnableImageSupportInUploadFile", "X-GptId": "WordDraftingAgent@BuiltInAgents"},
            files=[(k, (None, v)) for k, v in [("scenario", "UploadImage"), ("conversationId", conv),
                   ("FileBase64", image_data), ("optionsSets", "cwcgptvsan"),
                   ("optionsSets", "flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch"),
                   ("optionsSets", "gptvnorm2048")]],
            proxies={"https": args.proxy}, timeout=40, allow_redirects=False)
        result["upload_status"] = response.status_code
        if not response.ok:
            print(json.dumps(result))
            return 1
        uploaded = response.json()
        result["upload_response_keys"] = list(uploaded)
        result["uploaded_type"] = uploaded.get("fileType")
        result["sanitizer"] = uploaded.get("fileSanitizer")
        result["upload_result"] = uploaded.get("result", {}).get("value")
        result["has_doc_id"] = bool(uploaded.get("docId"))
        if not result["has_doc_id"] or result["upload_result"] != "Success":
            print(json.dumps(result))
            return 1
        conv = uploaded.get("conversationId") or conv
        params = {"access_token": cred["access_token"], "ClientRequestId": str(uuid.uuid4()),
                  "source": "word", "scenario": cred.get("scenario", ""),
                  "ConversationId": conv, "variants": "agt_worddraftingagent_enable_image_rich_response"}
        query = urllib.parse.urlencode(params)
        proxy = urllib.parse.urlparse(args.proxy)
        ws = websocket.create_connection("wss://substrate.office.com/m365Copilot/ChatHub?" + query,
            timeout=60, origin="https://fa000000125.resources.office.net", http_proxy_host=proxy.hostname,
            http_proxy_port=proxy.port, proxy_type="http")
        send({"protocol": "json", "version": 1})
        ack = json.loads(ws.recv().split("\x1e")[0])
        if ack.get("error"):
            result["error"] = "handshake_rejected"
            print(json.dumps(result))
            return 1
        mid = str(uuid.uuid4())
        msg = {"author": "user", "inputMethod": "Keyboard", "messageType": "Chat",
               "text": "Read the 8-character code in the attached image. Reply only with that code. Do not read or edit an Office document.",
               "messageId": mid, "requestId": mid, "locale": "en-US", "experienceType": "Default"}
        msg["messageAnnotations"] = [{"messageAnnotationType": "ImageFile", "id": uploaded["docId"],
            "messageAnnotationMetadata": {"@type": "File", "annotationType": "File", "fileType": "png", "fileName": "probe.png"}}]
        payload = {"source": "word", "isStartOfSession": False,
                   "traceId": uuid.uuid4().hex, "tone": "Claude_Opus",
                   "gpts": [{"id": "WordDraftingAgent", "source": "BuiltInAgents", "version": "helix"}], "message": msg,
                   "optionsSets": ["cwc_flux_image", "cwcfluxgptv", "flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch", "gptvnorm2048"]}
        send({"type": 4, "invocationId": "0", "target": "chat", "arguments": [payload]})
        threading.Thread(target=ping, daemon=True).start()
        texts = {}
        start = time.monotonic()
        plugins = 0
        done = False
        while not done and time.monotonic() - start < 120:
            for record in ws.recv().split("\x1e"):
                if not record:
                    continue
                frame = json.loads(record)
                if frame.get("target") == "invoke_local_plugin":
                    plugins += 1
                    if plugins > 16:
                        raise RuntimeError("plugin limit")
                    arg = frame["arguments"][0]
                    try:
                        result.setdefault("plugin_methods", []).append(json.loads(arg.get("invocation", {}).get("payload", "{}")).get("method"))
                    except (ValueError, TypeError):
                        pass
                    send({"type": 3, "invocationId": frame["invocationId"], "result": {
                        "schema_version": arg["schema_version"], "correlation_id": arg["correlation_id"],
                        "response": {"status": "Fail", "message": "No Office document is connected. Document tools are unavailable."}}})
                updates = frame.get("arguments", []) if frame.get("target") == "update" else []
                if frame.get("type") == 2:
                    updates = [frame.get("item", {})]
                for update in updates:
                    if isinstance(update.get("result"), dict):
                        result["result"] = update["result"].get("value")
                    for message in update.get("messages", []):
                        if message.get("author") == "bot" and message.get("messageType", "Chat") in ("Chat", ""):
                            texts[message.get("messageId", "")] = message.get("text", "")
                if frame.get("type") in (3, 7):
                    result["completion_type"] = frame["type"]
                    result["has_error"] = bool(frame.get("error"))
                    done = True
        result["image_code_correct"] = any(expected in t.upper() for t in texts.values())
        result["text_chars"] = sum(map(len, texts.values()))
        result["synthetic_probe_reply"] = list(texts.values())
        result["plugin_calls"] = plugins
    except Exception as exc:
        result["error"] = type(exc).__name__
        result["error_cause"] = type(exc.__cause__ or exc.__context__).__name__
    finally:
        stop.set()
        if ws:
            ws.close()
    print(json.dumps(result))
    return 0 if result.get("image_code_correct") and result.get("result") == "Success" and result.get("completion_type") == 3 and not result.get("has_error") else 1


if __name__ == "__main__":
    sys.exit(main())
