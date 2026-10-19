package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

type testRawVerifier struct {
	bodies  map[string][]byte
	changed bool
}

func (v testRawVerifier) verify(_ context.Context, key string, body []byte) (objectProof, error) {
	stored, ok := v.bodies[key]
	if !ok || !bytes.Equal(stored, body) {
		return objectProof{}, errors.New("missing or different object")
	}
	return objectProof{Key: key, Generation: 1, Bytes: int64(len(body))}, nil
}

func (v testRawVerifier) unchanged(context.Context, objectProof) error {
	if v.changed {
		return errors.New("generation changed")
	}
	return nil
}

func TestVerifyRawCopyPreservesUnverifiedBodies(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		response                                     []byte
		changed, reportFails, partial, wrongLocation bool
	}{
		{name: "corrupt response", response: []byte("bad")},
		{name: "missing response"},
		{name: "changed generation", response: []byte("response"), changed: true},
		{name: "proof not durable", response: []byte("response"), reportFails: true},
		{name: "partially cleared row", response: []byte("response"), partial: true},
		{name: "different bucket", response: []byte("response"), wrongLocation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := rawCopy{ID: "r", Version: "1", Location: "gcs://bucket/prod", RequestKey: "req", ResponseKey: "resp", Request: []byte("request"), Response: []byte("response")}
			if tc.partial {
				row.Request = nil
			}
			if tc.wrongLocation {
				row.Location = "gcs://other/prod"
			}
			v := testRawVerifier{bodies: map[string][]byte{"req": []byte("request"), "resp": tc.response}, changed: tc.changed}
			cleared := false
			_, err := verifyRawCopy(context.Background(), row, "gcs://bucket/prod", v, true,
				func(any) error {
					if tc.reportFails {
						return errors.New("disk full")
					}
					return nil
				},
				func(context.Context, rawCopy) error { cleared = true; return nil })
			if err == nil || cleared {
				t.Fatalf("err=%v cleared=%t", err, cleared)
			}
		})
	}
}

func TestVerifyRawCopyOrderAndReadOnly(t *testing.T) {
	for _, drop := range []bool{false, true} {
		row := rawCopy{ID: "r", Version: "1", Location: "gcs://bucket/prod", RequestKey: "req", ResponseKey: "resp", Request: []byte("request"), Response: []byte("response")}
		v := testRawVerifier{bodies: map[string][]byte{"req": row.Request, "resp": row.Response}}
		var events []string
		n, err := verifyRawCopy(context.Background(), row, row.Location, v, drop,
			func(event any) error { events = append(events, event.(map[string]any)["event"].(string)); return nil },
			func(context.Context, rawCopy) error { events = append(events, "update"); return nil })
		want := "verified"
		if drop {
			want = "verified,update,cleared"
		}
		got := ""
		for i, event := range events {
			if i > 0 {
				got += ","
			}
			got += event
		}
		if err != nil || n != 15 || got != want {
			t.Fatalf("drop=%t bytes=%d events=%s err=%v", drop, n, got, err)
		}
	}
}

func TestClearVerifiedRawSQL(t *testing.T) {
	dsn := os.Getenv("PIPROXY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PIPROXY_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, `CREATE TEMP TABLE conversation_payloads (
		request_id text PRIMARY KEY, body_location text, request_object_key text, response_object_key text,
		request_body_compressed bytea, response_body_compressed bytea, expires_at timestamptz)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []bool{false, true} {
		_, err := conn.Exec(ctx, `TRUNCATE conversation_payloads;
			INSERT INTO conversation_payloads VALUES ('r','gcs://bucket/prod','req','resp','request','response',NULL)`)
		if err != nil {
			t.Fatal(err)
		}
		var version string
		if err := conn.QueryRow(ctx, `SELECT xmin::text FROM conversation_payloads`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if mutate {
			if _, err := conn.Exec(ctx, `UPDATE conversation_payloads SET request_body_compressed='changed'`); err != nil {
				t.Fatal(err)
			}
		}
		tag, err := conn.Exec(ctx, clearVerifiedRawSQL, "r", version, "gcs://bucket/prod", "req", "resp")
		want := int64(1)
		if mutate {
			want = 0
		}
		if err != nil || tag.RowsAffected() != want {
			t.Fatalf("mutate=%t updated=%d err=%v", mutate, tag.RowsAffected(), err)
		}
		var cleared, archival bool
		if err := conn.QueryRow(ctx, `SELECT request_body_compressed IS NULL AND response_body_compressed IS NULL,
			expires_at IS NULL AND body_location='gcs://bucket/prod' AND request_object_key='req' AND response_object_key='resp'
			FROM conversation_payloads`).Scan(&cleared, &archival); err != nil {
			t.Fatal(err)
		}
		if cleared == mutate || !archival {
			t.Fatalf("mutate=%t cleared=%t archival=%t", mutate, cleared, archival)
		}
	}
}
