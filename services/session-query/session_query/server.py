import argparse
import fcntl
import hashlib
import hmac
import json
import logging
import signal
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from .config import Config
from .exporter import Exporter
from .postgres import Postgres
from .query import Engine, Unavailable
from .storage import Catalog, GCS


class Server(ThreadingHTTPServer):
    request_queue_size = 8

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.slots = threading.BoundedSemaphore(4)

    def process_request(self, request, address):
        if not self.slots.acquire(timeout=0.1):
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, address)
        except Exception:
            self.slots.release()
            raise

    def process_request_thread(self, request, address):
        try:
            super().process_request_thread(request, address)
        finally:
            self.slots.release()


def handler(engine, token, exporter=None):
    class Handler(BaseHTTPRequestHandler):
        def setup(self):
            super().setup()
            self.connection.settimeout(10)

        def log_message(self, *args):
            pass  # URLs and bodies may contain private session identifiers.

        def reply(self, code, body, content_type="application/json", manifest=None):
            self.send_response(code)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            if manifest is not None:
                self.send_header("X-Transcript-ID", manifest["transcript_id"])
                self.send_header("X-Transcript-Tenant", hashlib.sha256(manifest["tenant_id"].encode()).hexdigest())
                self.send_header("X-Transcript-Generation", str(manifest["generation"]))
                self.send_header("X-Transcript-Last-Seq", str(manifest["last_seq"]))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            if self.path in ("/healthz", "/readyz"):
                self.reply(200, b'{"status":"ok"}')
            elif self.path == "/metrics":
                values = {"session_query_requests_total": engine.requests,
                          "session_query_snapshot_misses_total": engine.misses,
                          "session_query_failures_total": engine.failures,
                          "session_query_export_successes_total": exporter.successes if exporter else 0,
                          "session_query_export_failures_total": exporter.failures if exporter else 0,
                          "session_query_source_races_total": exporter.source_races if exporter else 0,
                          "session_query_poll_failures_total": exporter.poll_failures if exporter else 0,
                          "session_query_last_poll_timestamp_seconds": exporter.last_poll_success if exporter else 0,
                          "session_query_last_export_timestamp_seconds": exporter.last_success if exporter else 0}
                self.reply(200, "".join(f"{key} {value}\n" for key, value in values.items()).encode(), "text/plain; version=0.0.4")
            else:
                self.reply(404, b'{"error":"not_found"}')

        def do_POST(self):
            if not hmac.compare_digest(self.headers.get("Authorization", "").encode(), ("Bearer " + token).encode()):
                self.reply(401, b'{"error":"unauthorized"}')
                return
            operation = {"/v1/page": "page", "/v1/search": "search", "/v1/summary": "summary"}.get(self.path)
            if operation is None:
                self.reply(404, b'{"error":"not_found"}')
                return
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if not 1 <= length <= 16384 or self.headers.get("Transfer-Encoding"):
                    raise ValueError("invalid body size")
                body = self.rfile.read(length)
                if len(body) != length:
                    raise ValueError("incomplete request")
                request = json.loads(body)
                if not isinstance(request, dict):
                    raise ValueError("expected object")
                data, manifest = engine.execute(operation, request)
                self.reply(200, data, "application/x-ndjson" if operation == "page" else "application/json", manifest)
            except (ValueError, KeyError, json.JSONDecodeError):
                self.reply(400, b'{"error":"invalid_query"}')
            except Unavailable:
                self.reply(409, b'{"error":"snapshot_unavailable"}')
            except (BrokenPipeError, ConnectionResetError):
                return
            except Exception:
                logging.warning("session query failed")
                self.reply(503, b'{"error":"query_unavailable"}')
    return Handler


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--export-once", action="store_true", help="export one full read-only inventory pass and exit")
    args = parser.parse_args()
    config = Config.from_env()
    catalog = Catalog(config, GCS(config))
    # One exporter/query process per cache volume; never race manifest publishers.
    lock = (catalog.root / ".service.lock").open("w")
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    exporter = Exporter(config, catalog, Postgres(config))
    if args.export_once:
        after = ""
        while batch := exporter.postgres.batch(after):
            for state in batch:
                exporter.export(state)
            after = batch[-1]["transcript_id"]
        return
    engine = Engine(config, catalog)
    server = Server((config.bind, config.port), handler(engine, config.token, exporter))
    stop = threading.Event()
    worker = threading.Thread(target=exporter.run, args=(stop,), daemon=True)
    worker.start()

    def shutdown(*_):
        stop.set()
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    try:
        server.serve_forever(poll_interval=0.2)
    finally:
        stop.set()
        server.server_close()
        worker.join(timeout=35)
        engine.close()
        lock.close()


if __name__ == "__main__":
    main()
