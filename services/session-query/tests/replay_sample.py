"""Replay an existing private selection.json/object-receipts.jsonl sample.

Run from this service directory: uv run --locked python tests/replay_sample.py
    --sample /path/to/sample --output /path/to/private/output
No production access or writes; source and derived data stay outside the repo.
"""
import argparse
import base64
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import sys
import threading
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from session_query.config import Config
from session_query.exporter import Exporter
from session_query.query import Engine
from session_query.server import Server, handler
from session_query.storage import Catalog, digest_file


class SampleObjects:
    def __init__(self, sample, output):
        self.sample, self.output = sample, output
        self.receipts = {"prod/" + r["key"]: r for r in
                         map(json.loads, (sample / "object-receipts.jsonl").read_text().splitlines())}
        self.derived = {}
        self.source_reads = self.cache_reads = 0

    def download(self, key, destination, max_bytes, generation=None):
        if key in self.receipts:
            proof = dict(self.receipts[key], key=key)
            proof["generation"] = int(proof["generation"])
            source = self.sample / "objects" / (str(proof["object_index"]) + ".gz")
            self.source_reads += 1
        else:
            source, proof = self.derived[key]
            self.cache_reads += 1
        sha, crc = digest_file(source)
        expected_crc = int.from_bytes(base64.b64decode(proof["crc32c"]), "big")
        if sha != proof["sha256"] or crc != expected_crc or source.stat().st_size != proof["size"]:
            raise ValueError("sample object integrity mismatch")
        if proof["size"] > max_bytes or generation is not None and generation != proof["generation"]:
            raise ValueError("sample object size/generation mismatch")
        shutil.copyfile(source, destination)
        return dict(proof)

    def put_immutable(self, key, path, content_type):
        sha, crc = digest_file(path)
        destination = self.output / "objects" / sha
        destination.parent.mkdir(exist_ok=True, mode=0o700)
        if destination.exists() and digest_file(destination)[0] != sha:
            raise ValueError("immutable conflict")
        shutil.copyfile(path, destination)
        proof = dict(key=key, generation=1, size=path.stat().st_size, sha256=sha,
                     crc32c=base64.b64encode(crc.to_bytes(4, "big")).decode())
        self.derived[key] = destination, proof
        check = path.with_suffix(".verify")
        try:
            self.download(key, check, proof["size"], 1)
            if digest_file(check)[0] != sha:
                raise ValueError("durable copy mismatch")
        finally:
            check.unlink(missing_ok=True)
        return dict(proof)


class Snapshot:
    def still_current(self, state):
        return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sample", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False, mode=0o700)
    sample = json.loads((args.sample / "selection.json").read_text())
    config = Config(root=args.output / "cache", token=secrets.token_hex(32))
    objects = SampleObjects(args.sample, args.output)
    catalog = Catalog(config, objects)
    exporter = Exporter(config, catalog, Snapshot())
    total_rows = total_turns = 0
    started = time.perf_counter()
    for session in sample["sessions"]:
        turns = []
        for turn in session["turns"]:
            usage = turn["usage"] or {}
            turns.append(dict(seq=turn["seq"], request_id=turn["request_id"],
                              segment_key=turn["segment_key"], segment_bytes=turn["segment_bytes"],
                              tool_names=turn["tool_names"] or [], input_tokens=usage.get("input_tokens", 0),
                              output_tokens=usage.get("output_tokens", 0),
                              cost_cents=str(usage.get("cost_total_cents", 0)), usage_present=bool(usage)))
        state = dict(session, transcript_id=session["id"], tenant_id=session["tenant_id"] or "", turns=turns)
        assert exporter.export(state)
        manifest = catalog.manifest(state["tenant_id"], state["transcript_id"])
        total_rows += manifest["rows"]
        total_turns += state["last_seq"]
    exported_seconds = time.perf_counter() - started
    engine = Engine(config, catalog)
    server = Server(("127.0.0.1", 0), handler(engine, config.token))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        # Exercise the real Go client, renderer and source reader over identical snapshots.
        repo = Path(__file__).resolve().parents[3]
        environment = dict(os.environ, SESSION_QUERY_REPLAY_TOKEN=config.token,
                           GOCACHE="/private/tmp/piproxy-go-build")
        go = subprocess.run(["go", "run", "./services/session-query/tests/replay",
                             str(args.sample), f"http://127.0.0.1:{server.server_port}"],
                            cwd=repo, env=environment, capture_output=True, text=True)
        if go.returncode:
            raise RuntimeError(go.stderr)
        result = json.loads(go.stdout)
        for session in sample["sessions"]:
            request = dict(tenant_id=session["tenant_id"] or "", transcript_id=session["id"],
                           generation=session["generation"], last_seq=session["last_seq"])
            summary = json.loads(engine.execute("summary", request)[0])
            assert sum(x["requests"] for x in summary) == session["last_seq"]
            assert sum(x["errors"] for x in summary) == sum(t["status"] == "error" for t in session["turns"])
            assert sum(x["input_tokens"] for x in summary) == sum((t["usage"] or {}).get("input_tokens", 0) for t in session["turns"])
            engine.execute("search", dict(request, term="error", limit=5))
        result.update(sessions=len(sample["sessions"]), turns=total_turns, events=total_rows,
                      source_objects_verified=objects.source_reads, export_seconds=exported_seconds,
                      parquet_chunks=len(list(catalog.chunks.glob("*.parquet"))),
                      cache_bytes=sum(p.stat().st_size for p in catalog.chunks.glob("*.parquet")),
                      summaries_and_searches_checked=len(sample["sessions"]),
                      engine_failures=engine.failures)
        (args.output / "results.json").write_text(json.dumps(result, indent=2) + "\n")
        print(json.dumps(result, indent=2))
    finally:
        server.shutdown()
        server.server_close()
        thread.join()
        engine.close()


if __name__ == "__main__":
    main()
