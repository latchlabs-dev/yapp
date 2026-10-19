import json
import threading
import time

import duckdb


class Unavailable(Exception):
    pass


class Engine:
    def __init__(self, config, catalog):
        self.config, self.catalog = config, catalog
        self.lock = threading.Lock()
        # This connection remains open for the process lifetime, preserving file caches.
        self.connection = duckdb.connect()
        c = self.connection
        c.execute("SET threads=2")
        c.execute("SET memory_limit='128MB'")
        c.execute("SET max_temp_directory_size='256MB'")
        c.execute("SET temp_directory=?", [str(catalog.work / "queries")])
        c.execute("SET allowed_directories=?", [[str(catalog.chunks) + "/"]])
        c.execute("SET enable_external_file_cache=true")
        c.execute("SET parquet_metadata_cache=true")
        c.execute("SET autoinstall_known_extensions=false")
        c.execute("SET autoload_known_extensions=false")
        c.execute("SET enable_external_access=false")
        c.execute("SET lock_configuration=true")
        self.requests = self.misses = self.failures = 0

    def validate(self, request, operation):
        required = {"tenant_id", "transcript_id", "generation", "last_seq"}
        optional = {"from_seq", "limit"} if operation == "page" else ({"term", "limit"} if operation == "search" else set())
        if set(request) - required - optional or not required <= set(request):
            raise ValueError("invalid query fields")
        for key in ("tenant_id", "transcript_id"):
            if not isinstance(request[key], str) or len(request[key]) > 256 or (key == "transcript_id" and not request[key]):
                raise ValueError("invalid identity")
        for key in ("generation", "last_seq", "from_seq", "limit"):
            if key in request and (type(request[key]) is not int or not 1 <= request[key] <= 2147483647):
                raise ValueError("invalid sequence/limit")
        if operation == "search" and (not isinstance(request.get("term"), str) or not 1 <= len(request["term"]) <= 256):
            raise ValueError("invalid search term")

    def execute(self, operation, request):
        self.validate(request, operation)
        if not self.lock.acquire(timeout=0.25):
            raise Unavailable("query capacity exhausted")
        timer = None
        try:
            self.requests += 1
            m = self.catalog.manifest(request["tenant_id"], request["transcript_id"])
            if m is None or m["generation"] != request["generation"] or m["last_seq"] != request["last_seq"]:
                self.misses += 1
                raise Unavailable("snapshot not exported")
            chunks = m["chunks"]
            params = [request["tenant_id"], request["transcript_id"], request["generation"]]
            scope = "tenant_id=? AND transcript_id=? AND generation=?"
            if operation == "page":
                start = request.get("from_seq", 1)
                limit = min(request.get("limit", 50), 200)
                end = min(request["last_seq"], start + limit - 1)
                lo = max(1, start - 1)  # Echo rendering needs the preceding turn.
                if start > request["last_seq"]:
                    return b"", m
                chunks = [x for x in chunks if x["start_seq"] <= end and x["end_seq"] >= lo]
                if sum(x["raw_bytes"] for x in chunks) > self.config.response_bytes:
                    raise Unavailable("selected chunks exceed query materialization budget")
                sql = f"SELECT raw_line FROM read_parquet(?) WHERE {scope} AND seq BETWEEN ? AND ? ORDER BY seq,event_index"
                params += [lo, end]
            elif operation == "search":
                sql = f"""SELECT seq,request_id,record_type,event_index,LEFT(search_text,500) AS preview
                    FROM read_parquet(?) WHERE {scope} AND contains(lower(search_text),lower(?))
                    ORDER BY seq,event_index LIMIT ?"""
                params += [request["term"], min(request.get("limit", 50), 100)]
            elif operation == "summary":
                sql = f"""SELECT provider,model,LEFT(created_at,10) AS day,COUNT(*) AS requests,
                    COUNT(*) FILTER(WHERE status='error') AS errors,SUM(input_tokens) AS input_tokens,
                    SUM(output_tokens) AS output_tokens,CAST(SUM(cost_cents) AS VARCHAR) AS cost_cents,
                    COUNT(*) FILTER(WHERE NOT usage_present) AS missing_usage
                    FROM read_parquet(?) WHERE {scope} AND record_type='turn'
                    GROUP BY ALL ORDER BY day,provider,model"""
            else:
                raise ValueError("unknown operation")
            if not chunks:
                raise Unavailable("missing snapshot coverage")
            # Catalog lock pins files; persistent engine lock prevents cross-request interrupts.
            with self.catalog.files(chunks) as files:
                timer = threading.Timer(self.config.query_seconds, self.connection.interrupt)
                timer.start()
                rows = self.connection.execute(sql, [files] + params)
                if operation == "page":
                    body = bytearray()
                    while row := rows.fetchone():
                        line = row[0].encode("utf8")
                        if len(body) + len(line) > self.config.response_bytes:
                            raise Unavailable("page exceeds response budget")
                        body.extend(line)
                    return bytes(body), m
                names = [column[0] for column in rows.description]
                return json.dumps([dict(zip(names, row)) for row in rows.fetchall()], separators=(",", ":")).encode(), m
        except Unavailable:
            raise
        except Exception:
            self.failures += 1
            raise
        finally:
            if timer:
                timer.cancel()
                timer.join()  # An old timeout cannot interrupt the next request.
            self.lock.release()

    def close(self):
        with self.lock:
            self.connection.close()
