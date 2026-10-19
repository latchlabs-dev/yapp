"""Optional integration check against a disposable DB with repository migrations.

PIPROXY_SESSION_QUERY_TEST_DSN must point to that disposable database as its
owner. This test creates/deletes only its own fixtures and temporary SELECT role.
"""
import os
from pathlib import Path
import unittest
import uuid

import psycopg
from psycopg import sql
from psycopg.conninfo import make_conninfo

from session_query.config import Config
from session_query.postgres import Postgres


@unittest.skipUnless(os.getenv("PIPROXY_SESSION_QUERY_TEST_DSN"), "disposable PostgreSQL DSN not set")
class PostgresTest(unittest.TestCase):
    def test_real_read_only_snapshot_usage_and_generation_races(self):
        name = "querytest_" + uuid.uuid4().hex
        bucket = "test-" + name
        base = os.environ["PIPROXY_SESSION_QUERY_TEST_DSN"]
        with psycopg.connect(base, autocommit=True) as admin:
            admin.execute(sql.SQL("CREATE ROLE {} LOGIN PASSWORD 'test-only'").format(sql.Identifier(name)))
            try:
                admin.execute(sql.SQL("GRANT SELECT ON session_transcripts,session_turns,usage_events TO {}").format(sql.Identifier(name)))
                for suffix, location, rebuilding in (("a", "gcs://" + bucket + "/prod", False),
                                                     ("b", "gcs://foreign/prod", False),
                                                     ("c", "gcs://" + bucket + "/prod", True)):
                    admin.execute("""INSERT INTO session_transcripts
                        (id,conversation_id,started_at,last_activity_at,last_seq,objects_location,needs_rebuild)
                        VALUES (%s,%s,now(),now(),2,%s,%s)""", (name + suffix, name + suffix, location, rebuilding))
                for seq in (1, 2):
                    admin.execute("""INSERT INTO session_turns
                        (transcript_id,generation,seq,request_id,created_at,provider,model,status,kind,base,
                        input_count,delta_count,output_count,tool_names,segment_key,segment_bytes)
                        VALUES (%s,1,%s,%s,now(),'p','m','success','extend',0,1,1,1,ARRAY['shell'],'key',1)""",
                                  (name + "a", seq, name + str(seq)))
                admin.execute("""INSERT INTO usage_events
                    (id,request_id,provider,model,upstream_provider,upstream_model,status,started_at,input_tokens,
                    output_tokens,cost_total_cents) VALUES (%s,%s,'p','m','p','m','success',now(),11,2,0.001234)""",
                              (name, name + "1"))
                cfg = Config(root=Path("unused"), token="x" * 32, bucket=bucket,
                             dsn=make_conninfo(base, user=name, password="test-only", application_name=name))
                pg = Postgres(cfg)
                states = pg.batch("")
                self.assertEqual([name + "a"], [s["transcript_id"] for s in states])
                state = states[0]
                self.assertEqual("", state["tenant_id"])
                self.assertEqual((11, "0.001234", True), tuple(state["turns"][0][k] for k in ("input_tokens", "cost_cents", "usage_present")))
                self.assertFalse(state["turns"][1]["usage_present"])
                self.assertTrue(pg.still_current(state))
                self.assertEqual([], pg.batch(name + "a"))
                # Closed connections leave no backend occupied during export I/O.
                self.assertEqual(0, admin.execute("SELECT count(*) FROM pg_stat_activity WHERE application_name=%s", (name,)).fetchone()[0])
                with pg.connect() as conn:
                    self.assertEqual("on", conn.execute("SHOW transaction_read_only").fetchone()["transaction_read_only"])
                    with self.assertRaises(psycopg.errors.ReadOnlySqlTransaction):
                        conn.execute("UPDATE session_transcripts SET last_seq=3 WHERE id=%s", (name + "a",))
                    conn.rollback()
                for assignment in ("last_seq=3", "last_seq=2,generation=2", "generation=1,needs_rebuild=true"):
                    admin.execute("UPDATE session_transcripts SET " + assignment + " WHERE id=%s", (name + "a",))
                    self.assertFalse(pg.still_current(state))
            finally:
                admin.execute("DELETE FROM session_transcripts WHERE id IN (%s,%s,%s)", (name + "a", name + "b", name + "c"))
                admin.execute("DELETE FROM usage_events WHERE id=%s", (name,))
                admin.execute(sql.SQL("DROP OWNED BY {}").format(sql.Identifier(name)))
                admin.execute(sql.SQL("DROP ROLE {}").format(sql.Identifier(name)))
