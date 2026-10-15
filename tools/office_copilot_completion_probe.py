"""Opt-in synthetic Word ChatHub completion probe; never prints credentials or text."""

import argparse
import collections
import json
from pathlib import Path
import threading
import time
import urllib.parse
import uuid

import websocket

from office_copilot_reasoning_probe import credentials


def probe(cred, prompt):
    report = {"frames": [], "plugin_calls": 0}
    ws = None
    stop = threading.Event()
    lock = threading.Lock()
    start = time.monotonic()

    def send(value):
        with lock:
            ws.send(json.dumps(value, separators=(",", ":")) + "\x1e")

    def ping():
        while not stop.wait(15):
            try:
                send({"type": 6})
            except Exception:
                return

    def shape(value):
        if isinstance(value, dict):
            return {k: shape(v) for k, v in value.items() if k not in ("access_token", "token", "cookie", "authorization")}
        if isinstance(value, list):
            return [shape(v) for v in value[:4]]
        if isinstance(value, str):
            return {"string_length": len(value)}
        return value

    try:
        query = urllib.parse.urlencode({"access_token": cred["access_token"], "ClientRequestId": str(uuid.uuid4()),
            "source": "word", "scenario": cred.get("scenario") or "ExcelSidepaneWebPremiumConsumerCopilot"})
        ws = websocket.create_connection("wss://substrate.office.com/m365Copilot/ChatHub?" + query,
            timeout=45, origin="https://fa000000125.resources.office.net")
        send({"protocol": "json", "version": 1})
        records = ws.recv().split("\x1e")
        if "error" in json.loads(records[0]):
            report["error"] = "handshake_rejected"
            return report
        message_id = str(uuid.uuid4())
        payload = {"source": "word", "isStartOfSession": True, "traceId": uuid.uuid4().hex,
            "tone": "Claude_Opus", "gpts": [{"id": "WordDraftingAgent", "source": "BuiltInAgents", "version": "helix"}],
            "message": {"author": "user", "inputMethod": "Keyboard", "messageType": "Chat", "text": prompt,
                "messageId": message_id, "requestId": message_id, "locale": "en-US", "experienceType": "Default",
                "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}}
        send({"type": 4, "invocationId": "0", "target": "chat", "arguments": [payload]})
        threading.Thread(target=ping, daemon=True).start()
        pending = records[1:]
        done = False
        while not done and time.monotonic() - start < 120:
            if not any(pending):
                raw = ws.recv()
                if not raw:
                    break
                pending = raw.split("\x1e")
            for record in pending:
                if not record:
                    continue
                frame = json.loads(record)
                if frame.get("target") == "invoke_local_plugin":
                    report["plugin_calls"] += 1
                    if report["plugin_calls"] > 16:
                        raise RuntimeError("plugin_limit")
                    arg = frame["arguments"][0]
                    send({"type": 3, "invocationId": frame["invocationId"], "result": {
                        "schema_version": arg["schema_version"], "correlation_id": arg["correlation_id"],
                        "response": {"status": "Fail", "message": "No Office document is connected to this API session. Document tools are unavailable."}}})
                    continue
                entry = {"type": frame.get("type"), "keys": list(frame), "chat_invocation": frame.get("invocationId") == "0",
                    "has_error": bool(frame.get("error")), "updates": []}
                updates = frame.get("arguments", []) if frame.get("target") == "update" else []
                if frame.get("type") == 2:
                    updates = [frame.get("item", {})]
                if frame.get("type") == 3 and "result" in frame:
                    entry["completion_result_shape"] = shape(frame["result"])
                for update in updates:
                    item = {"keys": list(update), "result": update.get("result", {}).get("value"), "messages": []}
                    for message in update.get("messages", []):
                        item["messages"].append({"author": message.get("author"), "messageType": message.get("messageType"),
                            "text_chars": len(message.get("text") or ""), "keys": list(message),
                            "adaptiveCards_shape": shape(message.get("adaptiveCards"))})
                    entry["updates"].append(item)
                report["frames"].append(entry)
                if frame.get("type") == 7 or (frame.get("type") == 3 and frame.get("invocationId") == "0"):
                    done = True
            pending = []
        report["completed"] = done
    except Exception as exc:
        report["error"] = type(exc).__name__
        report["http_status"] = getattr(exc, "status_code", None)
    finally:
        stop.set()
        if ws:
            ws.close()
        report["seconds"] = round(time.monotonic() - start, 2)
    return report


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--run-live", action="store_true", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    cred = credentials(root)
    results = {}
    for name, prompt in [("default_hi", "hi"), ("explicit_text", "Reply only with: connection test successful. This is a text-only API connection test; no Office document is connected. Do not call tools or edit a document.")]:
        results[name] = probe(cred, prompt)
        print(json.dumps({"case": name, "completed": results[name].get("completed"), "error": results[name].get("error"), "seconds": results[name].get("seconds")}), flush=True)
        args.output.write_text(json.dumps(results, ensure_ascii=False, indent=2), encoding="utf-8")
