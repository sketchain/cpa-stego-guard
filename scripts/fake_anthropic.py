#!/usr/bin/env python3
"""A fake api.anthropic.com for the cpa-stego-guard end-to-end test.

It serves HTTPS with a test certificate, answers Messages and count_tokens
requests with fixed payloads, and appends every request it receives to a JSONL
capture file: method, path, headers in wire order, and the exact body bytes
(base64), so the test can compare what CLIProxyAPI really sent upstream.
"""

import argparse
import base64
import gzip
import json
import ssl
import threading
import uuid
import zlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOCK = threading.Lock()


def decode_body(raw, encoding):
    encoding = (encoding or "").strip().lower()
    if encoding == "gzip":
        return gzip.decompress(raw)
    if encoding == "deflate":
        return zlib.decompress(raw)
    if encoding == "zstd" or encoding == "br":
        raise ValueError("unsupported request content-encoding " + encoding)
    return raw


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    capture_path = ""

    def log_message(self, fmt, *args):
        pass

    def record(self, body):
        entry = {
            "method": self.command,
            "path": self.path,
            "headers": [[k, v] for k, v in self.headers.items()],
            "body_b64": base64.b64encode(body).decode("ascii"),
        }
        with LOCK:
            with open(self.capture_path, "a", encoding="utf-8") as handle:
                handle.write(json.dumps(entry) + "\n")

    def send_json(self, status, payload):
        raw = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("request-id", "req_" + uuid.uuid4().hex[:24])
        self.end_headers()
        self.wfile.write(raw)

    def do_HEAD(self):
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        body = decode_body(raw, self.headers.get("Content-Encoding"))
        self.record(body)
        path = self.path.split("?", 1)[0]
        try:
            payload = json.loads(body or b"{}")
        except ValueError:
            self.send_json(400, {"type": "error", "error": {"type": "invalid_request_error", "message": "bad json"}})
            return
        if path == "/v1/messages/count_tokens":
            self.send_json(200, {"input_tokens": 42})
            return
        if path != "/v1/messages":
            self.send_json(404, {"type": "error", "error": {"type": "not_found_error", "message": path}})
            return
        model = payload.get("model", "claude")
        message_id = "msg_" + uuid.uuid4().hex[:24]
        usage = {"input_tokens": 42, "output_tokens": 3, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
        if not payload.get("stream"):
            self.send_json(200, {
                "id": message_id, "type": "message", "role": "assistant", "model": model,
                "content": [{"type": "text", "text": "OK"}],
                "stop_reason": "end_turn", "stop_sequence": None, "usage": usage,
            })
            return
        events = [
            ("message_start", {"type": "message_start", "message": {
                "id": message_id, "type": "message", "role": "assistant", "model": model, "content": [],
                "stop_reason": None, "stop_sequence": None, "usage": dict(usage, output_tokens=1)}}),
            ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
            ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "OK"}}),
            ("content_block_stop", {"type": "content_block_stop", "index": 0}),
            ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                               "usage": {"output_tokens": 3}}),
            ("message_stop", {"type": "message_stop"}),
        ]
        raw_events = b"".join(
            ("event: %s\ndata: %s\n\n" % (name, json.dumps(data))).encode("utf-8") for name, data in events
        )
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(raw_events)))
        self.send_header("request-id", "req_" + uuid.uuid4().hex[:24])
        self.end_headers()
        self.wfile.write(raw_events)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=443)
    parser.add_argument("--cert", required=True)
    parser.add_argument("--key", required=True)
    parser.add_argument("--capture", required=True)
    args = parser.parse_args()
    Handler.capture_path = args.capture
    server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(args.cert, args.key)
    context.set_alpn_protocols(["http/1.1"])
    server.socket = context.wrap_socket(server.socket, server_side=True)
    print("fake anthropic listening", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
