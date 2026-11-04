from __future__ import annotations

import argparse
import json
import os
import signal
import socket
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import urlsplit

from .parsers import MAX_INPUT_BYTES, ParserFailure, parse_document, warmup


MAX_ACTIVE_REQUESTS = max(1, int(os.getenv("PARSER_MAX_CONCURRENT", "2")))
_request_slots = threading.BoundedSemaphore(MAX_ACTIVE_REQUESTS)


class ParserHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    request_queue_size = 16


class ParserHandler(BaseHTTPRequestHandler):
    server_version = "DocumentParser/1"
    sys_version = ""

    def log_message(self, format: str, *args: Any) -> None:
        print(f"parser-http {self.client_address[0]} {format % args}", flush=True)

    def do_GET(self) -> None:
        if urlsplit(self.path).path != "/health":
            self._send(404, {"code": "not_found", "message": "not found"})
            return
        self._send(200, {"status": "ok"})

    def do_POST(self) -> None:
        if urlsplit(self.path).path != "/parse":
            self._send(404, {"code": "not_found", "message": "not found"})
            return
        if not _request_slots.acquire(blocking=False):
            self._send(503, {"code": "ai.parse.busy", "message": "parser is at capacity"})
            return
        try:
            self._parse()
        finally:
            _request_slots.release()

    def _parse(self) -> None:
        try:
            content_length = int(self.headers.get("Content-Length", "-1"))
        except ValueError:
            self._send(400, {"code": "ai.parse.invalid_request", "message": "invalid content length"})
            return
        if content_length <= 0:
            self._send(422, {"code": "ai.parse.invalid_content", "message": "empty document"})
            return
        if content_length > MAX_INPUT_BYTES:
            self._send(413, {"code": "ai.parse.resource_limit", "message": "input exceeds parser limit"})
            return
        if self.headers.get("Transfer-Encoding", "identity").lower() != "identity":
            self._send(400, {"code": "ai.parse.invalid_request", "message": "transfer encoding is not supported"})
            return

        try:
            self.connection.settimeout(10)
            data = self.rfile.read(content_length)
            if len(data) != content_length:
                raise InvalidRequest("incomplete request body")
            size_limit = _bounded_header_int(self.headers, "X-Size-Limit-Bytes", MAX_INPUT_BYTES)
            page_limit = _bounded_header_int(self.headers, "X-Page-Limit", 1000)
            result = parse_document(
                data,
                self.headers.get("X-Parser-Media-Type", ""),
                size_limit_bytes=size_limit,
                page_limit=page_limit,
            )
        except InvalidRequest as error:
            self._send(400, {"code": "ai.parse.invalid_request", "message": str(error)})
            return
        except socket.timeout:
            self._send(408, {"code": "ai.parse.timeout", "message": "request body timed out"})
            return
        except ParserFailure as error:
            self._send(error.status_code, {"code": error.code, "message": error.code})
            return
        except Exception:
            self._send(500, {"code": "ai.parse.failed", "message": "document parsing failed"})
            return
        self._send(200, result)

    def _send(self, status: int, value: dict[str, Any]) -> None:
        body = json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass


class InvalidRequest(ValueError):
    pass


def _bounded_header_int(headers: Any, name: str, default: int) -> int:
    value = headers.get(name)
    if value is None or not value.strip():
        return default
    try:
        parsed = int(value)
    except ValueError as error:
        raise InvalidRequest(f"invalid {name}") from error
    if parsed <= 0:
        return default
    return min(parsed, default)


def create_server(host: str, port: int) -> ParserHTTPServer:
    return ParserHTTPServer((host, port), ParserHandler)


def run(host: str, port: int) -> None:
    warmup()
    server = create_server(host, port)
    stop_event = threading.Event()
    signal.signal(signal.SIGTERM, lambda _signum, _frame: stop_event.set())

    def serve() -> None:
        try:
            server.serve_forever()
        finally:
            stop_event.set()

    serve_thread = threading.Thread(target=serve, name="parser-http", daemon=True)
    serve_thread.start()
    try:
        stop_event.wait()
    except KeyboardInterrupt:
        pass
    finally:
        server.shutdown()
        server.server_close()
        serve_thread.join(timeout=5)


def main() -> None:
    argument_parser = argparse.ArgumentParser(description="Document parsing worker")
    argument_parser.add_argument("--host", default="127.0.0.1:8090")
    arguments = argument_parser.parse_args()
    host, separator, port_text = arguments.host.rpartition(":")
    if not separator or not host:
        raise SystemExit("--host must be HOST:PORT")
    run(host, int(port_text))


if __name__ == "__main__":
    main()
