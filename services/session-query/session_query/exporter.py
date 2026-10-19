import gzip
import hashlib
import json
import logging
import shutil
import tempfile
import time
from pathlib import Path

import duckdb

from .storage import atomic_json, identity

SCHEMA = {k: "VARCHAR" for k in (
    "tenant_id", "transcript_id", "record_type", "request_id", "created_at", "provider",
    "model", "status", "kind", "search_text", "raw_line")}
SCHEMA.update(generation="INTEGER", seq="INTEGER", event_index="INTEGER", tool_names="VARCHAR[]",
              input_tokens="BIGINT", output_tokens="BIGINT", cost_cents="DECIMAL(18,6)", usage_present="BOOLEAN")


def searchable(value):
    if isinstance(value, str):
        return [] if value.startswith("data:image") else [value]
    if isinstance(value, list):
        return [s for v in value for s in searchable(v)]
    if not isinstance(value, dict):
        return []
    out = []
    for key in ("text", "content", "reasoning_content", "arguments", "output", "input", "summary"):
        if key in value:
            v = value[key]
            if key in ("arguments", "input") and isinstance(v, dict):
                out.append(json.dumps(v, ensure_ascii=False, separators=(",", ":")))
            else:
                out.extend(searchable(v))
    for call in value.get("tool_calls", []) or []:
        if isinstance(call, dict):
            out.extend(searchable(call.get("function", {})))
    return out


class Exporter:
    def __init__(self, config, catalog, postgres):
        self.config, self.catalog, self.postgres = config, catalog, postgres
        self.successes = self.failures = 0
        self.last_success = 0
        self.poll_failures = self.source_races = 0
        self.last_poll_success = 0
        self.stage = "metadata"

    def export(self, state):
        self.stage = "metadata"
        turns = state["turns"]
        if not 1 <= state["last_seq"] <= self.config.max_turns:
            raise ValueError("transcript exceeds metadata budget")
        if [t["seq"] for t in turns] != list(range(1, state["last_seq"] + 1)):
            raise ValueError("incomplete transcript metadata")
        metadata_hash = hashlib.sha256(json.dumps(turns, sort_keys=True, default=str).encode()).hexdigest()
        old = self.catalog.manifest(state["tenant_id"], state["transcript_id"])
        if old and (old["generation"], old["last_seq"], old["metadata_hash"]) == (state["generation"], state["last_seq"], metadata_hash):
            return False
        if shutil.disk_usage(self.catalog.work).free < self.config.derived_bytes * 3:
            raise ValueError("insufficient export disk headroom")
        sources = []
        if state.get("merged_key"):
            sources.append((state["merged_key"], state["merged_bytes"]))
        sources.extend((t["segment_key"], t["segment_bytes"]) for t in turns if t["seq"] > state["merged_through_seq"])
        con = duckdb.connect()
        con.execute("SET threads=1")
        con.execute("SET memory_limit='256MB'")
        con.execute("SET max_temp_directory_size='512MB'")
        try:
            with tempfile.TemporaryDirectory(dir=self.catalog.work) as directory:
                work = Path(directory)
                con.execute("SET temp_directory=?", [str(work / "spill")])
                receipts, chunks, seen = [], [], []
                total = derived_total = event_index = 0
                current = stream = chunk_path = None
                chunk_start = chunk_end = chunk_rows = 0
                decoded_hash = hashlib.sha256()
                for n, (key, expected_size) in enumerate(sources):
                    self.stage = "source_download"
                    source = work / (str(n) + ".gz")
                    full_key = "/".join(p for p in (self.config.source_prefix, key) if p)
                    proof = self.catalog.objects.download(full_key, source, self.config.source_bytes)
                    if proof["size"] != expected_size:
                        raise ValueError("source size differs from PostgreSQL snapshot")
                    source_hash = hashlib.sha256()
                    self.stage = "source_decode"
                    with gzip.open(source, "rb") as f:
                        while line := f.readline(self.config.source_bytes + 1):
                            if len(line) > self.config.source_bytes:
                                raise ValueError("oversized JSONL record")
                            total += len(line)
                            if total > self.config.decoded_bytes:
                                raise ValueError("decoded transcript exceeds export budget")
                            source_hash.update(line)
                            decoded_hash.update(line)
                            record = json.loads(line)
                            kind = record["t"]
                            if kind not in ("turn", "input", "output", "system", "error"):
                                raise ValueError("unknown transcript record version")
                            if kind == "turn":
                                current = record
                                seq = current["seq"]
                                if seq != len(seen) + 1 or current.get("v", 1) != 1 or current["request_id"] != turns[seq - 1]["request_id"]:
                                    raise ValueError("source sequence/request/version mismatch")
                                seen.append(seq)
                                if stream is not None and (seq - 1) % self.config.chunk_turns == 0:
                                    stream.close()
                                    chunks.append(self.write_chunk(con, work, chunk_path, chunk_start, chunk_end, chunk_rows))
                                    stream = None
                                if stream is None:
                                    chunk_start, chunk_rows = seq, 0
                                    chunk_path = work / ("chunk-" + str(seq) + ".ndjson")
                                    stream = chunk_path.open("w", encoding="utf8")
                                chunk_end = seq
                            if current is None:
                                raise ValueError("record precedes turn")
                            t = turns[current["seq"] - 1]
                            text = record.get("text", "") if kind in ("system", "error") else "\n".join(searchable(record.get("raw")))
                            row = {k: current.get(k, "") for k in ("request_id", "created_at", "provider", "model", "status", "kind")}
                            row.update(tenant_id=state["tenant_id"], transcript_id=state["transcript_id"], generation=state["generation"],
                                       seq=current["seq"], event_index=event_index, record_type=kind, search_text=text,
                                       raw_line=line.decode("utf8"), tool_names=t["tool_names"] if kind == "turn" else [],
                                       input_tokens=t["input_tokens"] if kind == "turn" else 0,
                                       output_tokens=t["output_tokens"] if kind == "turn" else 0,
                                       cost_cents=t["cost_cents"] if kind == "turn" else "0",
                                       usage_present=t["usage_present"] if kind == "turn" else False)
                            derived_line = json.dumps(row, ensure_ascii=False, separators=(",", ":")).encode("utf8")
                            derived_total += len(derived_line) + 1
                            if len(derived_line) + 1 > self.config.source_bytes or derived_total > self.config.derived_bytes:
                                raise ValueError("derived transcript exceeds export budget")
                            stream.write(derived_line.decode("utf8") + "\n")
                            event_index += 1
                            chunk_rows += 1
                    proof["decoded_sha256"] = source_hash.hexdigest()
                    receipts.append(proof)
                    source.unlink()
                if stream is not None:
                    stream.close()
                    chunks.append(self.write_chunk(con, work, chunk_path, chunk_start, chunk_end, chunk_rows))
                if len(seen) != state["last_seq"]:
                    raise ValueError("source coverage differs from snapshot")
                self.stage = "snapshot_check"
                if not self.postgres.still_current(state):
                    self.source_races += 1
                    return False  # A newer generation/turn is picked up next cycle.
                self.stage = "manifest_publish"
                manifest = dict(parser_version=1, duckdb_version=duckdb.__version__, tenant_id=state["tenant_id"],
                                transcript_id=state["transcript_id"], generation=state["generation"], last_seq=state["last_seq"],
                                metadata_hash=metadata_hash, rows=event_index, decoded_sha256=decoded_hash.hexdigest(),
                                exported_at=time.time(), source_objects=receipts, chunks=chunks)
                path = work / "manifest.json"
                atomic_json(path, manifest)
                manifest_sha = hashlib.sha256(path.read_bytes()).hexdigest()
                key = f"{self.config.parquet_prefix}/manifests/{identity(state['tenant_id'], state['transcript_id'])}/g{state['generation']}/{state['last_seq']}-{manifest_sha}.json"
                self.catalog.objects.put_immutable(key, path, "application/json")
                self.catalog.publish(manifest)
                return True
        finally:
            if "stream" in locals() and stream is not None:
                stream.close()
            con.close()

    def write_chunk(self, con, work, path, start, end, row_count):
        self.stage = "parquet_write"
        # Immutable content addressing deduplicates sealed chunks across snapshots.
        content_hash = hashlib.sha256(("parser-1/duckdb-" + duckdb.__version__).encode())
        with path.open("rb") as f:
            while block := f.read(1024 * 1024):
                content_hash.update(block)
        with path.open("rb") as f:
            maximum_record = max(1024 * 1024, max(map(len, f)))
        if maximum_record > self.config.source_bytes:
            raise ValueError("derived JSON record exceeds export budget")
        content_id = content_hash.hexdigest()
        parquet = work / (content_id + ".parquet")
        con.execute("""COPY (SELECT * FROM read_json($source,format='newline_delimited',columns=$columns,maximum_object_size=$maximum_record)
            ORDER BY seq,event_index) TO $target (FORMAT PARQUET, COMPRESSION ZSTD, COMPRESSION_LEVEL 3, ROW_GROUP_SIZE 2048)""",
                    {"source": str(path), "columns": SCHEMA, "target": str(parquet), "maximum_record": maximum_record})
        # Verify exact source lines AND all typed/query fields from the durable representation.
        expected = con.execute("SELECT * FROM read_json(?,format='newline_delimited',columns=?,maximum_object_size=?) ORDER BY seq,event_index",
                               [str(path), SCHEMA, maximum_record])
        actual = con.cursor()
        actual.execute("SELECT * FROM read_parquet(?) ORDER BY seq,event_index", [str(parquet)])
        rows = 0
        while batch := expected.fetchmany(128):
            if batch != actual.fetchmany(128):
                raise ValueError("Parquet round-trip mismatch")
            rows += len(batch)
        if actual.fetchone() is not None or rows != row_count:
            raise ValueError("Parquet row-count mismatch")
        actual.close()
        raw_bytes = con.execute("SELECT SUM(octet_length(encode(raw_line))) FROM read_parquet(?)", [str(parquet)]).fetchone()[0]
        key = f"{self.config.parquet_prefix}/chunks/{content_id}.parquet"
        proof = self.catalog.objects.put_immutable(key, parquet, "application/vnd.apache.parquet")
        proof.update(start_seq=start, end_seq=end, rows=rows, raw_bytes=raw_bytes)
        path.unlink()
        parquet.unlink()
        self.stage = "source_decode"
        return proof

    def run(self, stop):
        while not stop.is_set():
            after = ""
            try:
                while not stop.is_set():
                    batch = self.postgres.batch(after)
                    self.last_poll_success = time.time()
                    if not batch:
                        break
                    for state in batch:
                        if stop.is_set():
                            return
                        try:
                            if self.export(state):
                                self.successes += 1
                                self.last_success = time.time()
                        except Exception as error:
                            # Source merges delete superseded derived objects after
                            # commit. A changed DB snapshot is retried as a race,
                            # rather than claiming that current data is corrupt.
                            try:
                                changed = not self.postgres.still_current(state)
                            except Exception:
                                changed = False
                            if changed:
                                self.source_races += 1
                                continue
                            self.failures += 1
                            fingerprint = identity(state["tenant_id"], state["transcript_id"])[:16]
                            logging.warning("session Parquet export failed type=%s stage=%s session_hash=%s; source reader remains available",
                                            type(error).__name__, self.stage, fingerprint)
                    after = batch[-1]["transcript_id"]
                    stop.wait(0.1)
            except Exception as error:
                self.poll_failures += 1
                sqlstate = getattr(error, "sqlstate", None) or "none"
                # No raw exception messages, DSNs, records or credentials.
                logging.warning("session export polling failed type=%s sqlstate=%s", type(error).__name__, sqlstate)
            stop.wait(self.config.poll_seconds)
