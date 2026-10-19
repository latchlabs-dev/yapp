import gzip
import hashlib
import json
import pathlib
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer

from session_query.config import Config
from session_query.exporter import Exporter
from session_query.query import Engine, Unavailable
from session_query.server import handler
from session_query.storage import Catalog


class Objects:
    def __init__(self):
        self.data = {}
        self.downloads = 0

    def download(self, key, destination, max_bytes, generation=None):
        self.downloads += 1
        data = self.data[key]
        if len(data) > max_bytes:
            raise ValueError("oversized object")
        destination.write_bytes(data)
        return dict(key=key, generation=1, size=len(data), sha256=hashlib.sha256(data).hexdigest(), crc32c="test")

    def put_immutable(self, key, path, content_type):
        data = path.read_bytes()
        if key in self.data and self.data[key] != data:
            raise ValueError("immutable conflict")
        self.data[key] = data
        return dict(key=key, generation=1, size=len(data), sha256=hashlib.sha256(data).hexdigest(), crc32c="test")


class Postgres:
    current = True

    def still_current(self, state):
        return self.current


def snapshot(objects, count=5, tenant="tenant", transcript="transcript", generation=1):
    turns, lines = [], []
    for seq in range(1, count + 1):
        turns.append(dict(seq=seq, request_id=f"r{seq}", segment_key="unused", segment_bytes=0,
                          tool_names=["shell"], input_tokens=10, output_tokens=2, cost_cents="0.001000", usage_present=True))
        lines += [json.dumps(dict(t="turn", v=1, seq=seq, request_id=f"r{seq}", created_at="2026-10-09T00:00:00Z",
                                  provider="provider", model="model", status="error" if seq == 3 else "success", kind="extend", base=seq, input_count=seq+1)) + "\n",
                  json.dumps(dict(t="input", i=seq, raw={"role": "user", "content": f"hello timeout {seq}"})) + "\n",
                  json.dumps(dict(t="output", raw={"role": "assistant", "content": f"reply {seq}"})) + "\n"]
    key = "source-" + transcript
    raw = "".join(lines).encode()
    objects.data["prod/" + key] = gzip.compress(raw)
    return dict(tenant_id=tenant, transcript_id=transcript, generation=generation, last_seq=count,
                merged_key=key, merged_bytes=len(objects.data["prod/" + key]), merged_through_seq=count, turns=turns), raw


class ServiceTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.cfg = Config(root=pathlib.Path(self.temp.name), token="x"*32, chunk_turns=2)
        self.objects, self.pg = Objects(), Postgres()
        self.catalog = Catalog(self.cfg, self.objects)
        self.exporter = Exporter(self.cfg, self.catalog, self.pg)
        self.engine = Engine(self.cfg, self.catalog)
        self.state, self.raw = snapshot(self.objects)
        self.request = dict(tenant_id="tenant", transcript_id="transcript", generation=1, last_seq=5)

    def tearDown(self):
        self.engine.close()
        self.temp.cleanup()

    def test_changed_snapshot_retries_without_false_corruption_warning(self):
        from unittest.mock import patch
        class Stop:
            done = False
            def is_set(self): return self.done
            def wait(self, _): self.done = True
        self.pg.batch = lambda after: [self.state] if not after else []
        self.pg.current = False
        with patch.object(self.exporter, "export", side_effect=ValueError("private source message")), patch("logging.warning") as warning:
            self.exporter.run(Stop())
        self.assertEqual(1, self.exporter.source_races)
        self.assertEqual(0, self.exporter.failures)
        warning.assert_not_called()
        self.assertIsNone(self.catalog.manifest("tenant", "transcript"))

    def test_stable_export_failure_logs_stage_and_hash_without_private_message(self):
        from unittest.mock import patch
        class Stop:
            done = False
            def is_set(self): return self.done
            def wait(self, _): self.done = True
        self.pg.batch = lambda after: [self.state] if not after else []
        self.exporter.stage = "source_download"
        with patch.object(self.exporter, "export", side_effect=ValueError("private source message")), self.assertLogs(level="WARNING") as logs:
            self.exporter.run(Stop())
        self.assertEqual(1, self.exporter.failures)
        self.assertIn("stage=source_download", logs.output[0])
        self.assertIn("session_hash=", logs.output[0])
        self.assertNotIn("private source message", logs.output[0])

    def test_integrity_page_cache_search_summary(self):
        self.assertTrue(self.exporter.export(self.state))
        m = self.catalog.manifest("tenant", "transcript")
        self.assertEqual(hashlib.sha256(self.raw).hexdigest(), m["decoded_sha256"])
        self.assertEqual([(1, 2), (3, 4), (5, 5)], [(x["start_seq"], x["end_seq"]) for x in m["chunks"]])
        data, _ = self.engine.execute("page", dict(self.request, from_seq=3, limit=2))
        expected = b"".join(self.raw.splitlines(keepends=True)[3:12])  # previous turn 2 + requested turns 3,4
        self.assertEqual(data, expected)
        reads = self.objects.downloads
        self.assertEqual(data, self.engine.execute("page", dict(self.request, from_seq=3, limit=2))[0])
        self.assertEqual(reads, self.objects.downloads)
        summaries = json.loads(self.engine.execute("summary", self.request)[0])
        self.assertEqual((5, 1, 50, "0.005000"), (summaries[0]["requests"], summaries[0]["errors"], summaries[0]["input_tokens"], summaries[0]["cost_cents"]))
        search = json.loads(self.engine.execute("search", dict(self.request, term="TIMEOUT"))[0])
        self.assertEqual([1, 2, 3, 4, 5], [x["seq"] for x in search])
        self.assertFalse(self.exporter.export(self.state))

    def test_stale_snapshot_tenant_generation_and_query_injection(self):
        self.exporter.export(self.state)
        for field, value in (("tenant_id", "other"), ("generation", 2), ("last_seq", 6)):
            with self.assertRaises(Unavailable):
                self.engine.execute("page", dict(self.request, **{field: value}))
        with self.assertRaises(ValueError):
            self.engine.execute("page", dict(self.request, sql="SELECT * FROM read_csv('/etc/passwd')"))
        with self.assertRaises(ValueError):
            self.engine.execute("page", dict(self.request, from_seq=True))
        with self.assertRaises(Exception):
            self.engine.connection.execute("SELECT * FROM read_csv('/etc/passwd')").fetchall()

    def test_corrupt_cached_chunk_is_reverified_and_repaired(self):
        self.exporter.export(self.state)
        self.engine.execute("page", self.request)
        path = next(self.catalog.chunks.glob("*.parquet"))
        path.write_bytes(b"broken")
        before = self.objects.downloads
        self.assertEqual(self.raw, self.engine.execute("page", self.request)[0])
        self.assertGreater(self.objects.downloads, before)

    def test_racing_generation_and_incomplete_source_never_publish(self):
        self.pg.current = False
        self.assertFalse(self.exporter.export(self.state))
        self.assertIsNone(self.catalog.manifest("tenant", "transcript"))
        self.pg.current = True
        self.state["turns"].pop()
        with self.assertRaises(ValueError):
            self.exporter.export(self.state)
        self.assertIsNone(self.catalog.manifest("tenant", "transcript"))

    def test_source_corruption_and_response_budget_never_return_partial_data(self):
        key = "prod/" + self.state["merged_key"]
        original = self.objects.data[key]
        self.objects.data[key] = original[:-8] + b"bad-tail"
        with self.assertRaises((gzip.BadGzipFile, EOFError)):
            self.exporter.export(self.state)
        self.assertIsNone(self.catalog.manifest("tenant", "transcript"))
        self.objects.data[key] = original
        self.exporter.export(self.state)
        from dataclasses import replace
        limited = Engine(replace(self.cfg, response_bytes=1), self.catalog)
        try:
            with self.assertRaises(Unavailable):
                limited.execute("page", self.request)
        finally:
            limited.close()

    def test_cache_evicts_unpinned_chunks_and_late_usage_refreshes(self):
        self.exporter.export(self.state)
        self.engine.execute("page", dict(self.request, from_seq=1, limit=1))
        previous = self.catalog.manifest("tenant", "transcript")
        self.state["turns"][0]["input_tokens"] = 20
        self.assertTrue(self.exporter.export(self.state))
        latest = self.catalog.manifest("tenant", "transcript")
        self.assertNotEqual(previous["metadata_hash"], latest["metadata_hash"])
        self.assertNotEqual(previous["chunks"][0], latest["chunks"][0])
        self.assertEqual(previous["chunks"][1:], latest["chunks"][1:])
        self.assertEqual(60, json.loads(self.engine.execute("summary", self.request)[0])[0]["input_tokens"])
        cached = list(self.catalog.chunks.glob("*.parquet"))
        pinned = cached[0]
        self.catalog.evict(self.cfg.cache_bytes - pinned.stat().st_size, {pinned})
        self.assertEqual([pinned], list(self.catalog.chunks.glob("*.parquet")))

    def test_manifest_survives_restart_and_chunks_are_deduplicated(self):
        self.exporter.export(self.state)
        original = self.catalog.manifest("tenant", "transcript")
        state, _ = snapshot(self.objects, count=6)
        self.exporter.export(state)
        latest = self.catalog.manifest("tenant", "transcript")
        self.assertEqual(original["chunks"][:2], latest["chunks"][:2])
        restarted = Catalog(self.cfg, self.objects)
        self.assertEqual(latest, restarted.manifest("tenant", "transcript"))
        engine = Engine(self.cfg, restarted)
        try:
            self.assertEqual(b"", engine.execute("page", dict(self.request, last_seq=6, from_seq=7))[0])
        finally:
            engine.close()

    def test_http_auth_snapshot_headers_and_no_sql_endpoint(self):
        self.exporter.export(self.state)
        server = ThreadingHTTPServer(("127.0.0.1", 0), handler(self.engine, self.cfg.token))
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        url = f"http://127.0.0.1:{server.server_port}/v1/page"
        try:
            req = urllib.request.Request(url, data=json.dumps(self.request).encode())
            with self.assertRaises(urllib.error.HTTPError) as e:
                urllib.request.urlopen(req)
            self.assertEqual(401, e.exception.code)
            req.add_header("Authorization", "Bearer " + self.cfg.token)
            with urllib.request.urlopen(req) as response:
                self.assertEqual(self.raw, response.read())
                self.assertEqual("5", response.headers["X-Transcript-Last-Seq"])
                self.assertEqual("transcript", response.headers["X-Transcript-ID"])
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == "__main__":
    unittest.main()
