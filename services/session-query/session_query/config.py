import os
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class Config:
    root: Path
    token: str
    bucket: str = ""
    source_prefix: str = "prod"
    parquet_prefix: str = "session-parquet/v1"
    dsn: str = ""
    cache_bytes: int = 1024**3
    source_bytes: int = 64 * 1024**2
    decoded_bytes: int = 256 * 1024**2
    derived_bytes: int = 512 * 1024**2
    response_bytes: int = 64 * 1024**2
    max_turns: int = 10000
    chunk_turns: int = 128
    query_seconds: float = 5
    poll_seconds: float = 30
    batch: int = 10
    bind: str = "0.0.0.0"
    port: int = 8092

    @classmethod
    def from_env(cls):
        c = cls(
            root=Path(os.getenv("SESSION_QUERY_DATA_DIR", "/var/lib/session-query")),
            token=os.getenv("PROXY_SESSION_QUERY_TOKEN", ""),
            bucket=os.getenv("PROXY_SESSION_OBJECTS_BUCKET", ""),
            source_prefix=os.getenv("PROXY_SESSION_OBJECTS_PREFIX", "prod").strip("/"),
            parquet_prefix=os.getenv("SESSION_QUERY_PARQUET_PREFIX", "session-parquet/v1").strip("/"),
            dsn=os.getenv("SESSION_QUERY_POSTGRES_DSN", ""),
            cache_bytes=int(os.getenv("SESSION_QUERY_CACHE_BYTES", str(1024**3))),
            poll_seconds=float(os.getenv("SESSION_QUERY_POLL_SECONDS", "30")),
            bind=os.getenv("SESSION_QUERY_BIND", "0.0.0.0"),
            port=int(os.getenv("SESSION_QUERY_PORT", "8092")),
        )
        if len(c.token) < 32 or not c.bucket or not c.dsn:
            raise ValueError("a 32-character service token, GCS bucket and read-only PostgreSQL DSN are required")
        if c.cache_bytes < c.source_bytes or c.poll_seconds < 1:
            raise ValueError("cache must hold at least one source object; polling must be at least one second")
        if not c.parquet_prefix or c.parquet_prefix == c.source_prefix or ".." in c.parquet_prefix.split("/"):
            raise ValueError("Parquet must use a separate, safe object prefix")
        return c
