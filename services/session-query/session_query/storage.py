"""Immutable GCS objects and a bounded, verified local Parquet cache."""
import contextlib
import hashlib
import json
import os
import re
import threading
from pathlib import Path

import google_crc32c
from google.api_core.exceptions import PreconditionFailed
from google.cloud import storage


def digest_file(path):
    sha = hashlib.sha256()
    crc = google_crc32c.Checksum()
    with path.open("rb") as stream:
        while block := stream.read(1024 * 1024):
            sha.update(block)
            crc.update(block)
    return sha.hexdigest(), int.from_bytes(crc.digest(), "big")


def identity(tenant, transcript):
    # Caller-supplied IDs never become paths or SQL identifiers.
    return hashlib.sha256(json.dumps([tenant, transcript], separators=(",", ":")).encode()).hexdigest()


def atomic_json(path, value):
    tmp = path.with_suffix(".tmp")
    with tmp.open("w") as f:
        json.dump(value, f, separators=(",", ":"))
        f.flush()
        os.fsync(f.fileno())
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)


class GCS:
    def __init__(self, config):
        self.bucket = storage.Client().bucket(config.bucket)

    def download(self, key, destination, max_bytes, generation=None):
        blob = self.bucket.get_blob(key, generation=generation, timeout=20)
        if blob is None or blob.content_encoding or blob.size > max_bytes:
            raise ValueError("missing, encoded or oversized source object")
        pinned = int(blob.generation)
        blob.download_to_filename(str(destination), if_generation_match=pinned,
                                  checksum="crc32c", timeout=30)
        sha, crc = digest_file(destination)
        import base64
        expected_crc = int.from_bytes(base64.b64decode(blob.crc32c), "big")
        if destination.stat().st_size != blob.size or crc != expected_crc:
            raise ValueError("object integrity mismatch")
        os.chmod(destination, 0o600)
        return dict(key=key, generation=pinned, size=blob.size, crc32c=blob.crc32c, sha256=sha)

    def put_immutable(self, key, path, content_type):
        sha, _ = digest_file(path)
        blob = self.bucket.blob(key)
        blob.metadata = {"sha256": sha, "parser_version": "1"}
        try:
            blob.upload_from_filename(str(path), content_type=content_type,
                                      if_generation_match=0, checksum="crc32c", timeout=30)
        except PreconditionFailed:
            blob.reload(timeout=20)
            if blob.size != path.stat().st_size or (blob.metadata or {}).get("sha256") != sha:
                raise ValueError("immutable object conflict")
        # Verify the durable copy before publishing any manifest referencing it.
        check = path.with_suffix(".verify")
        try:
            proof = self.download(key, check, path.stat().st_size, int(blob.generation))
            if proof["sha256"] != sha:
                raise ValueError("durable Parquet differs from local export")
            return proof
        finally:
            check.unlink(missing_ok=True)


class Catalog:
    def __init__(self, config, objects):
        self.config, self.objects = config, objects
        self.root = config.root.resolve()
        self.chunks = self.root / "chunks"
        self.manifests = self.root / "manifests"
        self.work = self.root / "work"
        for path in (self.root, self.chunks, self.manifests, self.work):
            path.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.lock = threading.RLock()
        self.verified = {}  # Per-process verification: recheck persisted files after a restart.
        self.evict()  # Honor a reduced cache budget after restart as well.

    def manifest(self, tenant, transcript):
        path = self.manifests / (identity(tenant, transcript) + ".json")
        try:
            m = json.loads(path.read_text())
        except FileNotFoundError:
            return None
        if m["tenant_id"] != tenant or m["transcript_id"] != transcript or m["parser_version"] != 1:
            raise ValueError("manifest identity/version mismatch")
        return m

    def publish(self, manifest):
        with self.lock:
            atomic_json(self.manifests / (identity(manifest["tenant_id"], manifest["transcript_id"]) + ".json"), manifest)

    @contextlib.contextmanager
    def files(self, chunks):
        # Pin files through query completion; eviction cannot unlink a live query's files.
        with self.lock:
            paths = []
            for chunk in chunks:
                sha = chunk["sha256"]
                if not re.fullmatch("[0-9a-f]{64}", sha):
                    raise ValueError("invalid content hash")
                path = self.chunks / (sha + ".parquet")
                signature = (path.stat().st_size, path.stat().st_mtime_ns) if path.exists() else None
                if signature is None or signature != self.verified.get(sha):
                    valid = signature is not None and signature[0] == chunk["size"] and digest_file(path)[0] == sha
                    if not valid:
                        if chunk["size"] > self.config.cache_bytes:
                            raise ValueError("chunk exceeds cache budget")
                        self.evict(chunk["size"], set(paths))
                        tmp = path.with_suffix(".download")
                        try:
                            proof = self.objects.download(chunk["key"], tmp, chunk["size"], chunk["generation"])
                            if proof["sha256"] != sha or proof["size"] != chunk["size"]:
                                raise ValueError("cached Parquet integrity mismatch")
                            os.replace(tmp, path)
                        finally:
                            tmp.unlink(missing_ok=True)
                    self.verified[sha] = (path.stat().st_size, path.stat().st_mtime_ns)
                os.utime(path, None)
                self.verified[sha] = (path.stat().st_size, path.stat().st_mtime_ns)
                paths.append(path)
            yield [str(p) for p in paths]

    def evict(self, incoming=0, pinned=None):
        pinned = pinned or set()
        paths = sorted(self.chunks.glob("*.parquet"), key=lambda p: p.stat().st_mtime)
        total = sum(p.stat().st_size for p in paths)
        for path in paths:
            if total + incoming <= self.config.cache_bytes:
                break
            if path not in pinned:
                total -= path.stat().st_size
                path.unlink()
                self.verified.pop(path.stem, None)
        if total + incoming > self.config.cache_bytes:
            raise ValueError("selected chunks exceed cache budget")
