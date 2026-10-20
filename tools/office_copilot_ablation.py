"""One-request, memory-only diagnostic harness. Never executes generated tools.

Run beside observe.py on the server. Local root-only socket accepts bounded
diagnostic variants; raw requests and credentials never leave process memory.
"""
import copy
import hashlib
import json
import os
import resource
import socket
import threading
import subprocess
import pathlib
import urllib.parse
import urllib.error
import urllib.request

import observe

resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
SOCKET = os.path.join(os.path.dirname(__file__), os.environ.get("DIAGNOSTIC_SOCKET", "ablation.sock"))
captured = None
api_key = None
last_request = None
last_blocks = None
original_decode = observe.decode_http


def decode(buf):
    global captured, api_key
    result = original_decode(buf)
    if result and captured is None:
        first, headers, raw, _ = result
        key = headers.get(b"x-api-key") or headers.get(b"authorization", b"").removeprefix(b"Bearer ")
        if first.startswith(b"POST /v1/messages ") and hashlib.sha256(key).hexdigest() == os.environ["DIAGNOSTIC_KEY_HASH"]:
            try:
                body = json.loads(raw)
                if len(body.get("tools", [])) == 26 and observe.digest(body["tools"]) == "8b154bf80ae4fead0cfe":
                    captured, api_key = body, key.decode()
                    print(json.dumps({"event": "captured_in_memory", "body_bytes": len(raw), "metadata": observe.request_metadata(body)}), flush=True)
            except (ValueError, TypeError):
                pass
    return result


def probe(command):
    global last_request, last_blocks
    if captured is None:
        return {"state": "waiting_for_request"}
    if command.get("status"):
        return {"state": "ready", "metadata": observe.request_metadata(captured),
                "tools": [{"index": i, "name": t.get("name"), "description_chars": len(t.get("description", ""))}
                          for i, t in enumerate(captured.get("tools", []))]}
    if command.get("inspect_web_search"):
        return {"tool_definition": next((t for t in captured.get("tools", []) if t.get("name") == "web_search"), None)}
    body = copy.deepcopy(captured)
    if command.get("drop_system"):
        body.pop("system", None)
        body["messages"] = [m for m in body["messages"] if m.get("role") not in ("system", "developer")]
    if "system_blocks" in command:
        body["system"] = [body["system"][i] for i in command["system_blocks"]]
    if command.get("drop_tools"):
        body.pop("tools", None)
        body.pop("tool_choice", None)
    if "tool_indices" in command:
        body["tools"] = [body["tools"][i] for i in command["tool_indices"]]
    if command.get("drop_tool_descriptions"):
        for tool in body.get("tools", []):
            tool.pop("description", None)
    if command.get("minimal_tool_schema"):
        for tool in body.get("tools", []):
            tool["input_schema"] = {"type": "object", "properties": {"query": {"type": "string"}}, "required": ["query"]}
    if command.get("drop_invalid_null_defaults"):
        def clean_schema(value):
            if isinstance(value, dict):
                typ = value.get("type")
                excludes_null = isinstance(typ, str) and typ != "null" or isinstance(typ, list) and "null" not in typ
                if "default" in value and value["default"] is None and excludes_null:
                    value.pop("default")
                for v in value.values(): clean_schema(v)
            elif isinstance(value, list):
                for v in value: clean_schema(v)
        for tool in body.get("tools", []): clean_schema(tool.get("input_schema", {}))
    if command.get("plain_user"):
        body["messages"] = [{"role": "user", "content": "你好"}]
    if command.get("bridge_prefix"):
        # A diagnostic only: preserve every tool definition and message while
        # clarifying who executes the external tools, using an ordinary system
        # message. No native tool is executed by this harness.
        system = body.get("system") or []
        if isinstance(system, str): system = [{"type": "text", "text": system}]
        body["system"] = [{"type": "text", "text": command["bridge_prefix"]}] + system
    if command.get("tool_name_map"):
        for tool in body.get("tools", []):
            tool["name"] = command["tool_name_map"].get(tool.get("name"), tool.get("name"))
    if command.get("prompt"):
        body["messages"] = [{"role": "user", "content": command["prompt"]}]
    if "choice" in command:
        body["tool_choice"] = command["choice"]
    if command.get("web_search_description"):
        for tool in body.get("tools", []):
            if tool.get("name") == "web_search": tool["description"] = command["web_search_description"]
    if command.get("document_search_tool"):
        for tool in body.get("tools", []):
            if tool.get("name") == "web_search" and "description" in tool and "documentation" not in tool:
                tool["documentation"] = tool.pop("description")
    if command.get("tool_result"):
        if not last_request or not last_blocks:
            return {"case": command.get("case"), "error": "no_previous_tool_call"}
        body = copy.deepcopy(last_request)
        calls = [b for b in last_blocks if b.get("type") == "tool_use"]
        if not calls: return {"case": command.get("case"), "error": "no_previous_tool_call"}
        body.pop("tool_choice", None)
        body["messages"].extend([{"role": "assistant", "content": last_blocks}, {"role": "user", "content": [
            {"type": "tool_result", "tool_use_id": b["id"], "content": command["tool_result"]} for b in calls]}])
    if command.get("native_probe"):
        def db(sql):
            import shlex
            return subprocess.check_output(["docker", "exec", "sub2api-postgres", "sh", "-c",
                'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc ' + shlex.quote(sql)], text=True).strip()
        credentials = json.loads(db("SELECT credentials FROM accounts WHERE id=1105"))
        proxy = json.loads(db("SELECT row_to_json(p) FROM proxies p WHERE id=7"))
        auth = ""
        if proxy.get("username"):
            auth = urllib.parse.quote(proxy["username"], safe="") + ":" + urllib.parse.quote(proxy.get("password") or "", safe="") + "@"
        native = {"token": credentials["access_token"], "scenario": credentials.get("scenario") or "",
                  "proxy": proxy["protocol"] + "://" + auth + proxy["host"] + ":" + str(proxy["port"]),
                  "request": body, "tool_instructions": pathlib.Path(__file__).with_name("tool-instructions.txt").read_text()}
        if "web_search_enabled" in command: native["web_search"] = command["web_search_enabled"]
        proc = subprocess.run([str(pathlib.Path(__file__).with_name("native-probe"))], input=json.dumps(native), capture_output=True, text=True, timeout=140)
        if proc.returncode: return {"case": command.get("case"), "probe_error": True}
        return {"case": command.get("case"), **json.loads(proc.stdout)}
    raw = json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode()
    req = urllib.request.Request("http://127.0.0.1:28081/v1/messages", data=raw,
        headers={"x-api-key": api_key, "anthropic-version": "2023-06-01", "Content-Type": "application/json", "User-Agent": "office-copilot-ablation/1.0"})
    try:
        with urllib.request.urlopen(req, timeout=130) as response:
            result = response.read()
            text = []
            tool_calls = 0
            tool_names = []
            blocks = {}
            args = {}
            for line in result.splitlines():
                if line.startswith(b"data: "):
                    try:
                        event = json.loads(line[6:])
                        if event.get("type") == "content_block_start":
                            blocks[event["index"]] = event["content_block"]
                        if event.get("type") == "content_block_delta" and event.get("delta", {}).get("type") == "input_json_delta":
                            args[event["index"]] = args.get(event["index"], "") + event["delta"].get("partial_json", "")
                        part = event.get("delta", {}).get("text")
                        if isinstance(part, str): text.append(part)
                        if event.get("content_block", {}).get("type") == "tool_use":
                            tool_calls += 1
                            tool_names.append(event["content_block"].get("name"))
                    except (ValueError, TypeError): pass
            for index, raw_args in args.items(): blocks[index]["input"] = json.loads(raw_args)
            last_request, last_blocks = copy.deepcopy(body), [blocks[i] for i in sorted(blocks)]
            return {"case": command.get("case"), "status": response.status, "topic_refusal": observe.topic_refusal(result),
                    "text_chars": len("".join(text)), "tool_calls": tool_calls, "tool_names": tool_names, "has_stream_error": b'event: error' in result,
                    "tool_inputs": [b.get("input") for b in last_blocks if b.get("type") == "tool_use"],
                    "test_marker_returned": "COBALT_SEARCH_826" in "".join(text),
                    "request_sha256_prefix": observe.digest(body), "response_sha256_prefix": observe.digest(result)}
    except urllib.error.HTTPError as e:
        result = e.read()
        return {"case": command.get("case"), "status": e.code, "empty_completion": b"no assistant text" in result, "error_sha256_prefix": observe.digest(result)}
    except Exception as e:
        return {"case": command.get("case"), "error_type": type(e).__name__}


def server():
    with socket.socket(socket.AF_UNIX) as listener:
        listener.bind(SOCKET)
        os.chmod(SOCKET, 0o600)
        listener.listen(1)
        while True:
            conn, _ = listener.accept()
            with conn:
                conn.settimeout(150)
                try:
                    request = conn.recv(4096)
                    if not request:
                        continue
                    result = probe(json.loads(request))
                    if "case" in result: print(json.dumps({"event": "probe", **result}), flush=True)
                    conn.sendall(json.dumps(result).encode())
                except Exception as e:
                    try:
                        conn.sendall(json.dumps({"error_type": type(e).__name__}).encode())
                    except OSError:
                        pass


if __name__ == "__main__":
    observe.decode_http = decode
    threading.Thread(target=server, daemon=True).start()
    try:
        observe.main()
    finally:
        if os.path.exists(SOCKET): os.unlink(SOCKET)
