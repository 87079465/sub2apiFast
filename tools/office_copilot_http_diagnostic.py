"""Temporary metadata-only HTTP observer. No raw packets or credentials are saved.

Linux only; requires tcpdump. Stops after --seconds (default one hour).
Observes the selected API key by SHA256, supplied via --key-hash.
"""
import argparse
import collections
import datetime
import gzip
import hashlib
import json
import socket
import struct
import subprocess
import threading
import time


def digest(value):
    if not isinstance(value, bytes):
        value = json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode()
    return hashlib.sha256(value).hexdigest()[:20]


def shape(value):
    result = {"kind": type(value).__name__, "sha256_prefix": digest(value)}
    if isinstance(value, str):
        result.update(chars=len(value), greeting_only=value.strip() in ("你好", "hi", "hello"))
    elif isinstance(value, list):
        result["blocks"] = [
            {"type": b.get("type"), "text_chars": len(b.get("text", "")) if isinstance(b.get("text"), str) else 0,
             "greeting_only": b.get("text", "").strip() in ("你好", "hi", "hello") if isinstance(b.get("text"), str) else False}
            for b in value[:100] if isinstance(b, dict)
        ]
        result["count"] = len(value)
    return result


def request_metadata(body):
    messages = body.get("messages", [])
    if not isinstance(messages, list):
        messages = []
    tools = body.get("tools") or []
    return {
        "model": body.get("model"), "stream": body.get("stream", False),
        "top_level_fields": sorted(body), "system": shape(body.get("system")),
        "messages": [{"role": m.get("role"), "content": shape(m.get("content")),
                      "has_tool_calls": bool(m.get("tool_calls"))} for m in messages[:100] if isinstance(m, dict)],
        "message_count": len(messages), "tools_count": len(tools), "tools_sha256_prefix": digest(tools),
        "tool_choice": shape(body.get("tool_choice")), "body_sha256_prefix": digest(body),
    }


def topic_refusal(body):
    text = body.decode("utf-8", "replace")
    pieces = []
    for line in text.splitlines():
        if line.startswith("data: "):
            try:
                event = json.loads(line[6:])
                pieces.append(event.get("delta", {}).get("text", ""))
                for choice in event.get("choices", []):
                    pieces.append(choice.get("delta", {}).get("content") or "")
            except (ValueError, TypeError, AttributeError):
                pass
    combined = text + "".join(pieces)
    return "can't chat about this" in combined or "different topic" in combined


def decode_http(buf):
    end = buf.find(b"\r\n\r\n")
    if end < 0:
        return None
    lines = bytes(buf[:end]).split(b"\r\n")
    headers = {}
    for line in lines[1:]:
        if b":" in line:
            k, v = line.split(b":", 1)
            headers[k.lower()] = v.strip()
    pos = end + 4
    if b"chunked" in headers.get(b"transfer-encoding", b"").lower():
        chunks = []
        while True:
            line_end = buf.find(b"\r\n", pos)
            if line_end < 0:
                return None
            size = int(buf[pos:line_end].split(b";", 1)[0], 16)
            pos = line_end + 2
            if size == 0:
                if buf[pos:pos + 2] == b"\r\n":
                    pos += 2
                    break
                trailer = buf.find(b"\r\n\r\n", pos)
                if trailer < 0:
                    return None
                pos = trailer + 4
                break
            if len(buf) < pos + size + 2:
                return None
            chunks.append(bytes(buf[pos:pos + size]))
            pos += size + 2
        body = b"".join(chunks)
    elif b"content-length" in headers:
        size = int(headers[b"content-length"])
        if len(buf) < pos + size:
            return None
        body = bytes(buf[pos:pos + size])
        pos += size
    elif lines[0].startswith(b"HTTP/"):
        return None
    else:
        body = b""
    if headers.get(b"content-encoding") == b"gzip":
        body = gzip.decompress(body)
    return lines[0], headers, body, pos


class Observer:
    def __init__(self, key_hash, emit):
        self.key_hash, self.emit = key_hash, emit
        self.streams, self.pending = {}, collections.defaultdict(collections.deque)
        self.counter = 0

    def packet(self, packet, link):
        offset = {1: 14, 113: 16, 276: 20, 101: 0}.get(link)
        if offset is None or len(packet) < offset + 40 or packet[offset] >> 4 != 4:
            return
        ip = packet[offset:]
        if ip[9] != 6:
            return
        ihl = (ip[0] & 15) * 4
        tcp = ip[ihl:struct.unpack("!H", ip[2:4])[0]]
        if len(tcp) < 20:
            return
        src, dst, seq = struct.unpack("!HHI", tcp[:8])
        payload = tcp[(tcp[12] >> 4) * 4:]
        key = (socket.inet_ntoa(ip[12:16]), src, socket.inet_ntoa(ip[16:20]), dst)
        reverse = (key[2], dst, key[0], src)
        if tcp[13] & 2:
            self.streams.pop(key, None)
        if not payload:
            return
        if key not in self.streams:
            if not payload.startswith((b"POST ", b"HTTP/")):
                return
            self.streams[key] = [seq, bytearray(), {}, time.monotonic()]
        state = self.streams[key]
        expected, buf, waiting, _ = state
        state[3] = time.monotonic()
        if seq > expected:
            if len(waiting) < 128:
                waiting[seq] = payload
            return
        skip = expected - seq
        if skip < len(payload):
            buf.extend(payload[skip:])
            state[0] += len(payload) - skip
        for start in sorted(list(waiting)):
            if start > state[0]:
                break
            part = waiting.pop(start)
            skip = state[0] - start
            if skip < len(part):
                buf.extend(part[skip:])
                state[0] += len(part) - skip
        if len(buf) > 4 * 1024 * 1024:
            self.streams.pop(key, None)
            return
        try:
            while buf:
                parsed = decode_http(buf)
                if parsed is None:
                    break
                first, headers, body, size = parsed
                del buf[:size]
                if first.startswith(b"POST "):
                    auth = headers.get(b"x-api-key") or headers.get(b"authorization", b"").removeprefix(b"Bearer ")
                    selected = hashlib.sha256(auth).hexdigest() == self.key_hash
                    path = first.split(b" ")[1].split(b"?")[0].decode("ascii", "replace")
                    selected = selected and path in ("/v1/messages", "/v1/chat/completions", "/v1/responses")
                    self.counter += 1
                    request_id = self.counter if selected else None
                    self.pending[key].append(request_id)
                    if selected:
                        data = json.loads(body)
                        self.emit({"event": "request", "id": request_id, "path": path,
                                   "user_agent": headers.get(b"user-agent", b"").decode("ascii", "replace")[:180],
                                   "body_bytes": len(body), **request_metadata(data)})
                elif self.pending[reverse]:
                    request_id = self.pending[reverse].popleft()
                    if request_id is not None:
                        self.emit({"event": "response", "id": request_id, "status": int(first.split()[1]),
                                   "body_bytes": len(body), "sha256_prefix": digest(body),
                                   "topic_refusal": topic_refusal(body),
                                   "content_type": headers.get(b"content-type", b"").decode("ascii", "replace")})
        except (ValueError, TypeError, KeyError, IndexError, OSError):
            self.emit({"event": "parse_skipped"})
            self.streams.pop(key, None)
        if len(self.streams) > 256:
            oldest = sorted(self.streams, key=lambda k: self.streams[k][3])[:128]
            for old in oldest:
                self.streams.pop(old, None)
                self.pending.pop(old, None)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--key-hash", required=True)
    parser.add_argument("--hosts", nargs="+", required=True)
    parser.add_argument("--seconds", type=int, default=3600)
    args = parser.parse_args()
    def emit(value):
        value["time"] = datetime.datetime.now(datetime.timezone(datetime.timedelta(hours=8))).isoformat()
        print(json.dumps(value, ensure_ascii=True), flush=True)
    observer = Observer(args.key_hash, emit)
    capture = subprocess.Popen(["tcpdump", "-i", "any", "-n", "-s", "0", "-U", "-w", "-",
                                "tcp port 8080 and (" + " or ".join("host " + h for h in args.hosts) + ")"],
                               stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    timer = threading.Timer(args.seconds, capture.terminate)
    timer.daemon = True
    timer.start()
    try:
        header = capture.stdout.read(24)
        endian = "<" if header[:4] in (b"\xd4\xc3\xb2\xa1", b"\x4d\x3c\xb2\xa1") else ">"
        link = struct.unpack(endian + "I", header[20:24])[0]
        emit({"event": "started", "duration_seconds": args.seconds, "link_type": link})
        while True:
            header = capture.stdout.read(16)
            if len(header) != 16:
                break
            size = struct.unpack(endian + "IIII", header)[2]
            packet = capture.stdout.read(size)
            observer.packet(packet, link)
    finally:
        timer.cancel()
        capture.terminate()
        capture.wait()
        emit({"event": "stopped"})


if __name__ == "__main__":
    main()
