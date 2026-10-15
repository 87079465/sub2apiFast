"""Opt-in diagnostic for Word ChatHub reasoningEffort (not a supported setting).

Sends four short synthetic chats per selected model. Reads existing credentials
locally; never prints tokens, raw URLs, chat payloads, or model reasoning text.
Success alone does NOT demonstrate that an unknown request field takes effect.
Requires websocket-client and PyYAML. Does not start the local gateway.
"""

import argparse
import collections
import json
import os
from pathlib import Path
import re
import subprocess
import threading
import time
import urllib.parse
import uuid

import websocket
import yaml


def credentials(root):
    db = yaml.safe_load((root / "backend/config.yaml").read_text(encoding="utf-8"))["database"]
    env = dict(os.environ, PGPASSWORD=str(db["password"]), PGSSLMODE=str(db.get("sslmode", "disable")))
    result = subprocess.run(
        ["C:/pgsql/17/pgsql/bin/psql.exe", "-h", str(db["host"]), "-p", str(db["port"]),
         "-U", str(db["user"]), "-d", str(db["dbname"]), "-At", "-c",
         "SELECT credentials FROM accounts WHERE platform='office_copilot' AND deleted_at IS NULL AND id=63"],
        env=env, capture_output=True, text=True, encoding="utf-8",
    )
    if result.returncode or not result.stdout.strip():
        raise RuntimeError("Office credential lookup failed")
    return json.loads(result.stdout)


def probe(cred, model, variant, effort, placement="top-level"):
    if placement not in ("top-level", "app-context", "anthropic"):
        raise ValueError("Unknown diagnostic placement")
    aliases = {"Claude_Opus": "Opus55", "Claude_Sonnet": "Sonnet5"}
    if model not in aliases:
        raise ValueError("Unknown diagnostic model")
    summary = {"model": model, "variant": variant, "requested_effort": effort, "placement": placement}
    started = time.monotonic()
    ws = None
    stop = threading.Event()
    write_lock = threading.Lock()
    message_types = collections.Counter()
    texts = {}
    metadata = {}
    frames = collections.Counter()

    def scan(value, path=""):
        if isinstance(value, dict):
            for key, child in value.items():
                current = path + "." + key
                if key in ("reasoningEffort", "reasoning_effort", "reasoningLevel", "reasoning_tokens", "reasoningTokens", "budget_tokens", "effort"):
                    if isinstance(child, (int, float)) or child in ("none", "low", "medium", "high", "max", "invalid_probe_value"):
                        metadata[current] = child
                if isinstance(child, (dict, list)):
                    scan(child, current)
        elif isinstance(value, list):
            for child in value:
                scan(child, path + "[]")

    def send(value):
        # SignalR requires type before result in plugin completion frames.
        with write_lock:
            ws.send(json.dumps(value, separators=(",", ":")) + "\x1e")

    def ping():
        while not stop.wait(15):
            try:
                send({"type": 6})
            except Exception:
                return

    try:
        query = urllib.parse.urlencode({"access_token": cred["access_token"],
            "ClientRequestId": str(uuid.uuid4()), "source": "word",
            "scenario": cred.get("scenario") or "ExcelSidepaneWebPremiumConsumerCopilot"})
        ws = websocket.create_connection("wss://substrate.office.com/m365Copilot/ChatHub?" + query,
                                         timeout=35, origin="https://fa000000125.resources.office.net")
        send({"protocol": "json", "version": 1})
        ack = json.loads(ws.recv().split("\x1e")[0])
        if "error" in ack:
            summary["error"] = "handshake_rejected"
            return summary
        payload = {
            "source": "word", "isStartOfSession": True, "traceId": uuid.uuid4().hex,
            "tone": model, "gpts": [{"id": "WordDraftingAgent", "source": "BuiltInAgents", "version": "helix"}],
            "message": {"author": "user", "inputMethod": "Keyboard", "messageType": "Chat",
                        "text": "Compute 137 * 149. Reply only with the number. Do not edit a document or call tools.",
                        "messageId": str(uuid.uuid4()), "requestId": str(uuid.uuid4()),
                        "locale": "en-US", "experienceType": "Default"},
        }
        if placement == "app-context":
            # Experimental transplant of the observed OmniAgent annotation into
            # ChatHub. This is NOT a verified Claude reasoning control. Include
            # the same annotation without effort as the matched baseline.
            metadata_request = {"app": "word", "selectedModel": aliases[model]}
            if effort is not None:
                metadata_request["reasoningEffort"] = effort
            payload["message"]["messageAnnotations"] = [{
                "id": "app-context", "messageAnnotationType": "AppContext", "text": "",
                "messageAnnotationSource": "ClientAnnotated",
                "messageAnnotationMetadata": metadata_request,
            }]
        elif placement == "anthropic":
            # Anthropic's documented native API shape, experimentally sent to
            # Microsoft's ChatHub. Acceptance alone does not prove forwarding.
            payload["thinking"] = {"type": "adaptive"}
            if effort is not None:
                payload["output_config"] = {"effort": effort}
        elif effort is not None:
            payload["reasoningEffort"] = effort
        send({"type": 4, "invocationId": "0", "target": "chat", "arguments": [payload]})
        threading.Thread(target=ping, daemon=True).start()
        plugin_count = 0
        done = False
        while not done and time.monotonic() - started < 115:
            raw = ws.recv()
            if not raw:
                break
            for record in raw.split("\x1e"):
                if not record:
                    continue
                frame = json.loads(record)
                kind = frame.get("type")
                frames[str(kind)] += 1
                if frame.get("target") == "invoke_local_plugin":
                    plugin_count += 1
                    if plugin_count > 16:
                        raise RuntimeError("plugin_limit")
                    arg = frame["arguments"][0]
                    send({"type": 3, "invocationId": frame["invocationId"], "result": {
                        "schema_version": arg["schema_version"], "correlation_id": arg["correlation_id"],
                        "response": {"status": "Fail", "message": "No Office document is connected to this API session. Document tools are unavailable."}}})
                    continue
                scan(frame)
                updates = frame.get("arguments", []) if frame.get("target") == "update" else []
                if kind == 2:
                    updates = [frame.get("item", {})]
                for update in updates:
                    if isinstance(update.get("result"), dict):
                        summary["result"] = update["result"].get("value")
                    for message in update.get("messages", []):
                        if message.get("author") != "bot":
                            continue
                        msg_type = message.get("messageType", "Chat")
                        message_types[msg_type] += 1
                        if msg_type in ("Chat", "") and message.get("text"):
                            summary.setdefault("first_text_seconds", round(time.monotonic() - started, 2))
                            texts[message.get("messageId", "")] = message["text"]
                if kind == 7 or (kind == 3 and frame.get("invocationId") == "0"):
                    summary["completion_type"] = kind
                    summary["has_error"] = bool(frame.get("error"))
                    summary["completed"] = kind == 3 and not frame.get("error")
                    if isinstance(frame.get("error"), str):
                        # Retain diagnostic categories, never the raw error
                        # (which may include URLs, identifiers or request data).
                        error_text = frame["error"]
                        summary["error_types"] = sorted(set(re.findall(r"\b[A-Za-z][A-Za-z0-9]*(?:Exception|Error)\b", error_text)))
                        summary["error_markers"] = [word for word in
                            ("unauthorized", "expired", "forbidden", "deserialize", "convert", "annotation", "invalid", "argument")
                            if word in error_text.lower()]
                    done = True
        summary["plugin_calls"] = plugin_count
        summary["answer_correct"] = any("20413" in text.replace(",", "") for text in texts.values())
        summary["answer_chars"] = sum(map(len, texts.values()))
        summary["reasoning_metadata"] = metadata
        summary["message_types"] = dict(message_types)
        summary["frame_types"] = dict(frames)
    except Exception as exc:
        # Exception strings can contain the authenticated URL.
        summary["error"] = type(exc).__name__
        summary["http_status"] = getattr(exc, "status_code", None)
    finally:
        stop.set()
        if ws is not None:
            ws.close()
        summary["elapsed_seconds"] = round(time.monotonic() - started, 2)
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-live", action="store_true", help="Required: sends synthetic requests to Microsoft")
    parser.add_argument("--output", type=Path, required=True, help="Sanitized JSON result file")
    parser.add_argument("--model", choices=("Claude_Opus", "Claude_Sonnet"), help="Limit the probe to one route")
    parser.add_argument("--placement", choices=("top-level", "app-context", "anthropic"), default="top-level")
    args = parser.parse_args()
    if not args.run_live:
        parser.error("Explicit --run-live is required")
    cred = credentials(Path(__file__).resolve().parents[1])
    variants = [("baseline", None), ("low", "low"), ("high", "high"), ("invalid_control", "invalid_probe_value")]
    results = []
    for model in ([args.model] if args.model else ("Claude_Opus", "Claude_Sonnet")):
        for label, effort in variants:
            result = probe(cred, model, label, effort, args.placement)
            results.append(result)
            args.output.write_text(json.dumps(results, indent=2), encoding="utf-8")
            print(json.dumps(result), flush=True)
            if result.get("http_status") in (401, 403, 429):
                return


if __name__ == "__main__":
    main()
