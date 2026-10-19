package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/earlgrey/piproxy/internal/config"
)

type rawCopy struct {
	ID, Version, Location, RequestKey, ResponseKey string
	Request, Response                              []byte
}

type objectProof struct {
	Key        string `json:"key"`
	Generation int64  `json:"generation"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	CRC32C     uint32 `json:"crc32c"`
}

type rawProof struct {
	RequestID string      `json:"request_id"`
	Version   string      `json:"postgres_xmin"`
	Request   objectProof `json:"request"`
	Response  objectProof `json:"response"`
}

type rawVerifier interface {
	verify(context.Context, string, []byte) (objectProof, error)
	unchanged(context.Context, objectProof) error
}

type gcsRawVerifier struct {
	bucket *storage.BucketHandle
	prefix string
}

func (v *gcsRawVerifier) verify(ctx context.Context, key string, inline []byte) (objectProof, error) {
	name := key
	if v.prefix != "" {
		name = v.prefix + "/" + key
	}
	obj := v.bucket.Object(name)
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return objectProof{}, fmt.Errorf("object metadata %s: %w", name, err)
	}
	if attrs.ContentEncoding != "" {
		return objectProof{}, fmt.Errorf("object %s has unexpected content encoding %q", name, attrs.ContentEncoding)
	}
	if attrs.Size != int64(len(inline)) {
		return objectProof{}, fmt.Errorf("object %s size %d differs from inline %d", name, attrs.Size, len(inline))
	}
	reader, err := obj.Generation(attrs.Generation).NewReader(ctx)
	if err != nil {
		return objectProof{}, fmt.Errorf("open pinned object %s: %w", name, err)
	}
	defer reader.Close()
	// Bound memory even if object metadata or its reader is unexpectedly wrong.
	data, err := io.ReadAll(io.LimitReader(reader, int64(len(inline))+1))
	if err != nil {
		return objectProof{}, fmt.Errorf("read pinned object %s: %w", name, err)
	}
	if !bytes.Equal(data, inline) {
		return objectProof{}, fmt.Errorf("object %s differs from inline bytes", name)
	}
	checksum := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
	if checksum != attrs.CRC32C {
		return objectProof{}, fmt.Errorf("object %s CRC32C differs from GCS metadata", name)
	}
	hash := sha256.Sum256(data)
	return objectProof{Key: name, Generation: attrs.Generation, Bytes: attrs.Size, SHA256: hex.EncodeToString(hash[:]), CRC32C: checksum}, nil
}

func (v *gcsRawVerifier) unchanged(ctx context.Context, proof objectProof) error {
	attrs, err := v.bucket.Object(proof.Key).If(storage.Conditions{GenerationMatch: proof.Generation}).Attrs(ctx)
	if err != nil {
		return fmt.Errorf("recheck object %s: %w", proof.Key, err)
	}
	if attrs.Generation != proof.Generation || attrs.Size != proof.Bytes || attrs.CRC32C != proof.CRC32C || attrs.ContentEncoding != "" {
		return fmt.Errorf("object %s changed after verification", proof.Key)
	}
	return nil
}

// Never hold a transaction or row lock across object I/O. xmin prevents an
// update from clearing bodies that changed while their copies were checked.
const clearVerifiedRawSQL = `
	UPDATE conversation_payloads
	   SET request_body_compressed = NULL, response_body_compressed = NULL
	 WHERE request_id = $1 AND xmin::text = $2 AND body_location = $3
	   AND request_object_key = $4 AND response_object_key = $5
	   AND expires_at IS NULL
	   AND (request_body_compressed IS NOT NULL OR response_body_compressed IS NOT NULL)`

func verifyRawCopy(ctx context.Context, row rawCopy, location string, verifier rawVerifier, drop bool,
	record func(any) error, clear func(context.Context, rawCopy) error) (int64, error) {
	if row.Location != location || row.RequestKey == "" || row.ResponseKey == "" {
		return 0, errors.New("row has a different object location or missing keys")
	}
	if row.Request == nil || row.Response == nil {
		return 0, errors.New("both inline bodies are required to verify both archived copies")
	}
	proof := rawProof{RequestID: row.ID, Version: row.Version}
	var err error
	proof.Request, err = verifier.verify(ctx, row.RequestKey, row.Request)
	if err != nil {
		return 0, err
	}
	proof.Response, err = verifier.verify(ctx, row.ResponseKey, row.Response)
	if err != nil {
		return 0, err
	}
	if err := record(map[string]any{"event": "verified", "proof": proof, "at": time.Now().UTC()}); err != nil {
		return 0, fmt.Errorf("persist verification proof: %w", err)
	}
	if drop {
		for _, object := range []objectProof{proof.Request, proof.Response} {
			if err := verifier.unchanged(ctx, object); err != nil {
				return 0, err
			}
		}
		if err := clear(ctx, row); err != nil {
			return 0, err
		}
		if err := record(map[string]any{"event": "cleared", "request_id": row.ID, "at": time.Now().UTC()}); err != nil {
			return 0, fmt.Errorf("persist cleanup receipt (row was cleared): %w", err)
		}
	}
	return int64(len(row.Request) + len(row.Response)), nil
}

func rawInventory(ctx context.Context, pool *pgxpool.Pool) (map[string]any, error) {
	rows, err := pool.Query(ctx, `SELECT body_location, count(*),
		count(*) FILTER (WHERE request_body_compressed IS NOT NULL OR response_body_compressed IS NOT NULL),
		COALESCE(sum(COALESCE(octet_length(request_body_compressed),0) + COALESCE(octet_length(response_body_compressed),0)),0),
		count(*) FILTER (WHERE expires_at IS NOT NULL)
		FROM conversation_payloads GROUP BY body_location ORDER BY body_location`)
	if err != nil {
		return nil, err
	}
	var groups []map[string]any
	for rows.Next() {
		var location string
		var count, inline, size, expires int64
		if err := rows.Scan(&location, &count, &inline, &size, &expires); err != nil {
			rows.Close()
			return nil, err
		}
		groups = append(groups, map[string]any{"location": location, "rows": count, "inline_rows": inline, "inline_bytes": size, "expiring_rows": expires})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var tableBytes, databaseBytes, gaps int64
	if err := pool.QueryRow(ctx, `SELECT pg_total_relation_size('conversation_payloads'), pg_database_size(current_database()), (SELECT count(*) FROM capture_gaps)`).Scan(&tableBytes, &databaseBytes, &gaps); err != nil {
		return nil, err
	}
	return map[string]any{"groups": groups, "table_bytes": tableBytes, "database_bytes": databaseBytes, "capture_gaps": gaps, "at": time.Now().UTC()}, nil
}

func verifyRaw(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify-raw", flag.ContinueOnError)
	drop := fs.Bool("drop-postgres-bodies", false, "clear each unchanged row only after both GCS bodies match inline bytes")
	inventoryOnly := fs.Bool("inventory-only", false, "report body counts and sizes without reading GCS or changing rows")
	reportPath := fs.String("report", "", "new private JSONL audit report (required except for inventory)")
	maxRows := fs.Int("max-rows", 0, "maximum candidate rows (0 = all)")
	maxBytes := fs.Int64("max-inline-bytes", 0, "maximum inline bytes per run (0 = all; an oversized first row runs alone)")
	concurrency := fs.Int("concurrency", 8, "object verification workers (database pool is limited to two)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *concurrency < 1 || *concurrency > 32 || *maxRows < 0 || *maxBytes < 0 || fs.NArg() != 0 || (*drop && *inventoryOnly) {
		return errors.New("invalid verification flags")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.SessionArchive.ObjectsBackend != config.SessionObjectsGCS {
		return errors.New("verify-raw requires the configured GCS object backend")
	}
	// Maintenance must fail closed rather than falling back to PgBouncer.
	dsn := os.Getenv("PROXY_POSTGRES_DIRECT_DSN")
	if dsn == "" {
		return errors.New("PROXY_POSTGRES_DIRECT_DSN is required for archive maintenance")
	}
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return errors.New("cannot parse direct Postgres DSN")
	}
	if pc.ConnConfig.Port != 5432 {
		return errors.New("direct Postgres DSN must use port 5432")
	}
	pc.MaxConns, pc.MinConns = 2, 0
	pc.ConnConfig.RuntimeParams["application_name"] = "piproxy-archive-verify"
	pc.ConnConfig.RuntimeParams["statement_timeout"] = "30000"
	pc.ConnConfig.RuntimeParams["lock_timeout"] = "2000"
	if !*drop {
		pc.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return err
	}
	defer pool.Close()
	before, err := rawInventory(ctx, pool)
	if err != nil {
		return err
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"event": "inventory", "data": before})
	if *inventoryOnly {
		return nil
	}
	if *reportPath == "" {
		return errors.New("--report is required")
	}
	report, err := os.OpenFile(*reportPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer report.Close()
	var reportMu sync.Mutex
	record := func(event any) error {
		reportMu.Lock()
		defer reportMu.Unlock()
		if err := json.NewEncoder(report).Encode(event); err != nil {
			return err
		}
		return report.Sync()
	}
	if err := record(map[string]any{"event": "start", "inventory": before, "drop": *drop}); err != nil {
		return err
	}
	client, err := storage.NewClient(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	prefix := strings.Trim(cfg.SessionArchive.ObjectsPrefix, "/")
	location := "gcs://" + cfg.SessionArchive.ObjectsBucket
	if prefix != "" {
		location += "/" + prefix
	}
	verifier := &gcsRawVerifier{bucket: client.Bucket(cfg.SessionArchive.ObjectsBucket), prefix: prefix}
	// Fix a bounded candidate set; new captures are deliberately left for a
	// later pass. Only rows in this exact bucket/prefix are eligible.
	rows, err := pool.Query(ctx, `SELECT request_id,
		COALESCE(octet_length(request_body_compressed),0)::bigint + COALESCE(octet_length(response_body_compressed),0)::bigint
		FROM conversation_payloads
		WHERE body_location = $1 AND (request_body_compressed IS NOT NULL OR response_body_compressed IS NOT NULL)
		ORDER BY created_at, request_id LIMIT $2`, location, func() int {
		if *maxRows == 0 {
			return 2147483647
		}
		return *maxRows
	}())
	if err != nil {
		return err
	}
	var ids []string
	var candidateBytes int64
	for rows.Next() {
		var id string
		var size int64
		if err := rows.Scan(&id, &size); err != nil {
			rows.Close()
			return err
		}
		if *maxBytes > 0 && len(ids) > 0 && candidateBytes+size > *maxBytes {
			break
		}
		ids = append(ids, id)
		candidateBytes += size
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var mu sync.Mutex
	var verified, failed int
	var verifiedBytes int64
	started := time.Now()
	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				rowCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
				row := rawCopy{ID: id}
				err := pool.QueryRow(rowCtx, `SELECT xmin::text, body_location, COALESCE(request_object_key,''),
					COALESCE(response_object_key,''), request_body_compressed, response_body_compressed
					FROM conversation_payloads WHERE request_id=$1 AND expires_at IS NULL`, id).
					Scan(&row.Version, &row.Location, &row.RequestKey, &row.ResponseKey, &row.Request, &row.Response)
				var n int64
				if err == nil {
					n, err = verifyRawCopy(rowCtx, row, location, verifier, *drop, record, func(ctx context.Context, row rawCopy) error {
						tag, err := pool.Exec(ctx, clearVerifiedRawSQL, row.ID, row.Version, row.Location, row.RequestKey, row.ResponseKey)
						if err == nil && tag.RowsAffected() != 1 {
							return errors.New("row changed after verification; inline bodies preserved")
						}
						return err
					})
				}
				cancel()
				mu.Lock()
				if err != nil {
					failed++
					_ = record(map[string]any{"event": "failed", "request_id": id, "error": err.Error(), "at": time.Now().UTC()})
					fmt.Fprintf(os.Stderr, "verification failed request=%s: %v\n", id, err)
				} else {
					verified++
					verifiedBytes += n
				}
				if (verified+failed)%200 == 0 || verified+failed == len(ids) {
					fmt.Printf("candidates=%d verified=%d failed=%d bytes=%d drop=%t elapsed=%s\n", len(ids), verified, failed, verifiedBytes, *drop, time.Since(started).Round(time.Second))
				}
				mu.Unlock()
			}
		}()
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		jobs <- id
	}
	close(jobs)
	wg.Wait()
	after, err := rawInventory(ctx, pool)
	if err != nil {
		return err
	}
	summary := map[string]any{"event": "complete", "candidates": len(ids), "verified": verified, "failed": failed,
		"verified_bytes": verifiedBytes, "drop": *drop, "elapsed_seconds": time.Since(started).Seconds(), "inventory": after}
	if err := record(summary); err != nil {
		return err
	}
	_ = json.NewEncoder(os.Stdout).Encode(summary)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if failed > 0 {
		return fmt.Errorf("%d rows failed verification or cleanup; inspect the report", failed)
	}
	return nil
}
