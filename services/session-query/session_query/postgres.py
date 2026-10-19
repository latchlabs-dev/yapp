"""Short, bounded, read-only snapshots; never hold a backend during GCS I/O."""
import psycopg
from psycopg.rows import dict_row


class Postgres:
    def __init__(self, config):
        self.config = config

    def connect(self):
        return psycopg.connect(self.config.dsn, connect_timeout=5, row_factory=dict_row,
                              options="-c default_transaction_read_only=on -c statement_timeout=10000 -c lock_timeout=1000")

    def batch(self, after):
        location = "gcs://" + self.config.bucket + ("/" + self.config.source_prefix if self.config.source_prefix else "")
        with self.connect() as conn:
            conn.execute("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY")
            states = conn.execute("""SELECT id AS transcript_id, COALESCE(tenant_id,'') AS tenant_id,
                generation,last_seq,merged_key,merged_through_seq,merged_bytes
                FROM session_transcripts WHERE id>%s AND NOT needs_rebuild AND last_seq BETWEEN 1 AND %s
                AND objects_location=%s ORDER BY id LIMIT %s""", (after, self.config.max_turns, location, self.config.batch)).fetchall()
            for s in states:
                s["turns"] = conn.execute("""SELECT t.seq,t.request_id,t.segment_key,t.segment_bytes,
                    t.tool_names,COALESCE(u.input_tokens,0) AS input_tokens,
                    COALESCE(u.output_tokens,0) AS output_tokens,
                    COALESCE(u.cost_total_cents,0)::text AS cost_cents,
                    u.request_id IS NOT NULL AS usage_present
                    FROM session_turns t LEFT JOIN usage_events u USING(request_id)
                    WHERE t.transcript_id=%s AND t.generation=%s AND t.seq<=%s ORDER BY t.seq""",
                    (s["transcript_id"], s["generation"], s["last_seq"])).fetchall()
            return states

    def still_current(self, snapshot):
        with self.connect() as conn:
            row = conn.execute("SELECT generation,last_seq,needs_rebuild,COALESCE(tenant_id,'') AS tenant_id FROM session_transcripts WHERE id=%s",
                               (snapshot["transcript_id"],)).fetchone()
            return row and not row["needs_rebuild"] and row["generation"] == snapshot["generation"] and row["last_seq"] == snapshot["last_seq"] and row["tenant_id"] == snapshot["tenant_id"]
