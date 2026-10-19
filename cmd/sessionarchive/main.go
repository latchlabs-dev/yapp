// Command sessionarchive maintains the session archive (SESSION_ARCHIVE_PLAN.md):
//
//	sessionarchive backfill-transcripts  build transcripts for captured conversations
//	sessionarchive migrate-raw           move capture bodies from Postgres to object storage
//	sessionarchive migrate-pi-logs       move Pi session exports from Postgres to object storage
//	sessionarchive dry-run               build one transcript in memory, read-only, and report sizes
//
// It reads the same PROXY_* environment as the proxy and admin servers.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/config"
	"github.com/earlgrey/piproxy/internal/id"
	"github.com/earlgrey/piproxy/internal/sessionarchive"
	"github.com/earlgrey/piproxy/internal/sessionstorage"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "backfill-transcripts":
		err = backfillTranscripts(ctx, os.Args[2:])
	case "migrate-raw":
		err = migrate(ctx, os.Args[2:], false)
	case "migrate-pi-logs":
		err = migrate(ctx, os.Args[2:], true)
	case "dry-run":
		err = dryRun(ctx, os.Args[2:])
	case "check-objects":
		err = checkObjects(ctx, os.Args[2:])
	case "verify-raw":
		err = verifyRaw(ctx, os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessionarchive:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: sessionarchive <command> [flags]

commands:
  backfill-transcripts  build transcripts for conversations without a current one
  migrate-raw           copy capture bodies to object storage (then --drop-postgres-bodies)
  migrate-pi-logs       copy Pi session exports to object storage (then --drop-postgres-bodies)
  dry-run               build one conversation's transcript in memory, read-only
  check-objects         put, read back and delete a probe object (credentials check)
  verify-raw            verify existing GCS copies before optionally clearing inline bodies

Run "sessionarchive <command> -h" for flags.
`)
}

type env struct {
	cfg      *config.Config
	pool     *pgstore.Pool
	services *sessionstorage.Services
}

func (e *env) close() {
	_ = e.services.Close()
	e.pool.Close()
}

func open(ctx context.Context, readOnly bool) (*env, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	dsn := cfg.Postgres.DSN
	if readOnly {
		if dsn, err = readOnlyDSN(dsn); err != nil {
			return nil, err
		}
	}
	pool, err := pgstore.New(ctx, pgstore.Config{DSN: dsn, MaxOpenConns: 16, MaxIdleConns: 0})
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.TimeOnly}).With().Timestamp().Logger()
	services, err := sessionstorage.Open(ctx, cfg.SessionArchive, pool, logger)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return &env{cfg: cfg, pool: pool, services: services}, nil
}

// readOnlyDSN makes every session on the pool read-only, so a dry run pointed
// at production cannot write even through a bug.
func readOnlyDSN(dsn string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse postgres dsn: %w", err)
		}
		q := u.Query()
		q.Set("default_transaction_read_only", "on")
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	return dsn + " default_transaction_read_only=on", nil
}

// tenantFlag distinguishes "not set" (every tenant) from an explicit empty
// tenant (unauthenticated captures).
type tenantFlag struct {
	value *string
}

func (f *tenantFlag) String() string {
	if f.value == nil {
		return ""
	}
	return *f.value
}

func (f *tenantFlag) Set(v string) error {
	f.value = &v
	return nil
}

func backfillTranscripts(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backfill-transcripts", flag.ExitOnError)
	var tenant tenantFlag
	fs.Var(&tenant, "tenant", "only this tenant id (empty string = unauthenticated captures)")
	conversation := fs.String("conversation", "", "only this conversation id")
	since := fs.Duration("since", 0, "only conversations with captures in this window (e.g. 720h)")
	limit := fs.Int("limit", 100, "maximum conversations to process")
	concurrency := fs.Int("concurrency", 2, "conversations rebuilt in parallel")
	all := fs.Bool("all", false, "also rebuild conversations whose transcript is already current")
	dry := fs.Bool("dry-run", false, "list what would be rebuilt")
	_ = fs.Parse(args)

	e, err := open(ctx, *dry)
	if err != nil {
		return err
	}
	defer e.close()

	var refs []pgstore.ConversationRef
	if *conversation != "" {
		tenants, err := e.services.ConversationTenants(ctx, *conversation)
		if err != nil {
			return err
		}
		for _, ct := range tenants {
			if tenant.value == nil || *tenant.value == ct.TenantID {
				refs = append(refs, pgstore.ConversationRef{TenantID: ct.TenantID, ConversationID: *conversation, LastPayloadAt: ct.LastPayloadAt, RequestCount: ct.RequestCount})
			}
		}
	} else {
		filter := pgstore.BackfillFilter{TenantID: tenant.value, All: *all, Limit: *limit}
		if *since > 0 {
			cutoff := time.Now().Add(-*since)
			filter.Since = &cutoff
		}
		if refs, err = e.services.States.ConversationsNeedingTranscripts(ctx, filter); err != nil {
			return err
		}
	}

	totalRequests := 0
	for _, ref := range refs {
		totalRequests += ref.RequestCount
	}
	fmt.Printf("%d conversations, %d captured requests\n", len(refs), totalRequests)
	if *dry {
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "TENANT\tCONVERSATION\tREQUESTS\tLAST CAPTURE")
		for _, ref := range refs {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", displayTenant(ref.TenantID), ref.ConversationID, ref.RequestCount, ref.LastPayloadAt.Format(time.RFC3339))
		}
		return tw.Flush()
	}

	jobs := make(chan pgstore.ConversationRef)
	var mu sync.Mutex
	var failed, done int
	var wg sync.WaitGroup
	for i := 0; i < max(1, *concurrency); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ref := range jobs {
				started := time.Now()
				if _, err := e.services.Payloads.ArchiveConversation(ctx, ref.TenantID, ref.ConversationID); err != nil {
					mu.Lock()
					failed++
					done++
					fmt.Printf("[%d/%d] FAIL archival verification: %v\n", done, len(refs), err)
					mu.Unlock()
					continue
				}
				state, err := e.services.Builder.Rebuild(ctx, ref.TenantID, ref.ConversationID)
				mu.Lock()
				done++
				if err != nil {
					failed++
					fmt.Printf("[%d/%d] FAIL %s %s: %v\n", done, len(refs), displayTenant(ref.TenantID), ref.ConversationID, err)
				} else {
					fmt.Printf("[%d/%d] ok   %s %s requests=%d resets=%d transcript=%s elapsed=%s\n", done, len(refs),
						displayTenant(ref.TenantID), ref.ConversationID, state.RequestCount, state.ResetCount,
						humanBytes(state.TranscriptBytes()), time.Since(started).Round(time.Millisecond))
				}
				mu.Unlock()
			}
		}()
	}
	for _, ref := range refs {
		if ctx.Err() != nil {
			break
		}
		jobs <- ref
	}
	close(jobs)
	wg.Wait()
	if failed > 0 {
		return fmt.Errorf("%d of %d rebuilds failed", failed, len(refs))
	}
	return ctx.Err()
}

func migrate(ctx context.Context, args []string, piLogs bool) error {
	name := "migrate-raw"
	if piLogs {
		name = "migrate-pi-logs"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	var tenant tenantFlag
	fs.Var(&tenant, "tenant", "only this tenant id, e.g. for a pilot (empty string = unauthenticated captures)")
	batch := fs.Int("batch", 200, "rows selected per batch")
	maxRows := fs.Int("max-rows", 0, "stop after moving this many rows (0 = all)")
	concurrency := fs.Int("concurrency", 4, "rows moved in parallel")
	drop := fs.Bool("drop-postgres-bodies", false, "NULL the Postgres copy of bodies already in object storage")
	dry := fs.Bool("dry-run", false, "report how much would move")
	allowPostgres := fs.Bool("allow-postgres-objects", false, "allow the postgres object backend (testing only: bodies stay in Postgres)")
	_ = fs.Parse(args)

	e, err := open(ctx, *dry)
	if err != nil {
		return err
	}
	defer e.close()
	if e.cfg.SessionArchive.ObjectsBackend != config.SessionObjectsGCS && !*allowPostgres {
		return errors.New("migration moves bodies to object storage; set PROXY_SESSION_OBJECTS_BACKEND=gcs (or pass --allow-postgres-objects when testing)")
	}

	opts := pgstore.BodyMigrationOptions{TenantID: tenant.value, Batch: *batch, MaxRows: *maxRows, Concurrency: *concurrency, DropPostgresBodies: *drop, DryRun: *dry}
	started := time.Now()
	progress := func(s pgstore.BodyMigrationStats) {
		fmt.Printf("moved=%d bytes=%s dropped=%d elapsed=%s\n", s.Moved, humanBytes(s.Bytes), s.Dropped, time.Since(started).Round(time.Second))
	}
	var stats pgstore.BodyMigrationStats
	objects := e.services.Objects
	if piLogs {
		stats, err = pgstore.NewPiSessionLogStore(e.pool).WithObjects(objects, true, !*drop).MigrateToObjects(ctx, opts, progress)
	} else {
		stats, err = pgstore.NewConversationPayloadStore(e.pool).WithObjects(objects, true, !*drop).MigrateBodiesToObjects(ctx, opts, progress)
	}
	if *dry {
		fmt.Printf("pending rows=%d compressed bytes=%s → %s\n", stats.Pending, humanBytes(stats.Bytes), objects.Location())
		return err
	}
	fmt.Printf("done: moved=%d bytes=%s dropped=%d → %s\n", stats.Moved, humanBytes(stats.Bytes), stats.Dropped, objects.Location())
	if *drop {
		fmt.Println("Space is reclaimed only after VACUUM FULL or pg_repack on the table.")
	}
	return err
}

func dryRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dry-run", flag.ExitOnError)
	conversation := fs.String("conversation", "", "conversation id (required)")
	var tenant tenantFlag
	fs.Var(&tenant, "tenant", "tenant id (default: the most recently active tenant for the conversation)")
	merge := fs.Int("merge-segments", 0, "merge threshold (default: configured value)")
	pageSize := fs.Int("page", sessionarchive.DefaultPageTurns, "turns in the timed first page")
	_ = fs.Parse(args)
	if *conversation == "" {
		return errors.New("--conversation is required")
	}

	e, err := open(ctx, true)
	if err != nil {
		return err
	}
	defer e.close()

	tenantID := ""
	if tenant.value != nil {
		tenantID = *tenant.value
	} else {
		tenants, err := e.services.ConversationTenants(ctx, *conversation)
		if err != nil {
			return err
		}
		if len(tenants) == 0 {
			return fmt.Errorf("no captures for conversation %s", *conversation)
		}
		tenantID = tenants[0].TenantID
	}
	mergeSegments := e.cfg.SessionArchive.MergeSegments
	if *merge > 0 {
		mergeSegments = *merge
	}

	// A database that predates migration 028 (production before the archive
	// ships) cannot be read through the object-aware store.
	var migrated bool
	if err := e.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                WHERE table_name = 'conversation_payloads' AND column_name = 'body_location')
	`).Scan(&migrated); err != nil {
		return fmt.Errorf("detect schema: %w", err)
	}
	var source interface {
		sessionarchive.PayloadSource
		ConversationBodyBytes(ctx context.Context, tenantID, conversationID string) (int64, error)
	} = e.services.Payloads
	if !migrated {
		source = legacyPayloads{pool: e.pool}
	}

	// Everything the builder writes goes to memory; the database is only read.
	objects := blob.NewMemory()
	states := sessionarchive.NewMemoryStateStore()
	builder := sessionarchive.NewBuilder(objects, states, source, sessionarchive.BuilderOptions{MergeSegments: mergeSegments})

	started := time.Now()
	state, err := builder.Rebuild(ctx, tenantID, *conversation)
	if err != nil {
		return err
	}
	buildElapsed := time.Since(started)
	rawBytes, err := source.ConversationBodyBytes(ctx, tenantID, *conversation)
	if err != nil {
		return err
	}
	turns, _ := states.ListTurns(ctx, state.TranscriptID, state.Generation)
	kinds := map[string]int{}
	for _, turn := range turns {
		kinds[string(turn.Kind)]++
	}

	reader := sessionarchive.NewReader(objects)
	pageStarted := time.Now()
	page, err := reader.Page(ctx, state, turns, 1, *pageSize)
	if err != nil {
		return err
	}
	pageElapsed := time.Since(pageStarted)
	pageJSON, _ := json.Marshal(page)
	header, _ := json.Marshal(turns)

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	row := func(k string, v any) { fmt.Fprintf(tw, "%s\t%v\n", k, v) }
	row("conversation", *conversation)
	row("tenant", displayTenant(tenantID))
	row("schema", map[bool]string{true: "028+ (object-aware reader)", false: "pre-028 (bytea reader)"}[migrated])
	row("requests", state.RequestCount)
	row("turn kinds", formatKinds(kinds))
	row("user turns / tool calls / errors", fmt.Sprintf("%d / %d / %d", state.UserTurnCount, state.ToolCallCount, state.ErrorCount))
	row("opening prompt (runes)", len([]rune(state.OpeningPrompt)))
	row("raw bodies in postgres (gzip)", humanBytes(rawBytes))
	row("transcript objects (gzip)", fmt.Sprintf("%s in %d objects", humanBytes(objects.TotalBytes()), len(objects.Keys())))
	row("turn list JSON (session header)", humanBytes(int64(len(header))))
	row(fmt.Sprintf("first page JSON (%d turns)", len(page.Turns)), humanBytes(int64(len(pageJSON))))
	row("build elapsed", buildElapsed.Round(time.Millisecond))
	row("first page render", pageElapsed.Round(time.Millisecond))
	return tw.Flush()
}

// checkObjects proves the configured object store accepts writes, reads and
// deletes with the current credentials. Run it before pointing the proxy at a
// bucket: the proxy refuses to start when it cannot open its object store.
func checkObjects(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check-objects", flag.ExitOnError)
	_ = fs.Parse(args)

	e, err := open(ctx, false)
	if err != nil {
		return err
	}
	defer e.close()

	objects := e.services.Objects
	key := fmt.Sprintf("healthchecks/v1/%s.txt", id.New("probe"))
	payload := []byte("sessionarchive probe " + time.Now().UTC().Format(time.RFC3339Nano))
	if err := objects.Put(ctx, key, payload, "text/plain"); err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	got, err := objects.Read(ctx, key)
	if err != nil {
		return fmt.Errorf("read back %s: %w", key, err)
	}
	if !bytes.Equal(got, payload) {
		return fmt.Errorf("read back %s: got %d bytes, want %d", key, len(got), len(payload))
	}
	if err := objects.Delete(ctx, key); err != nil {
		return fmt.Errorf("delete probe %s: %w", key, err)
	}
	fmt.Printf("ok: put, read and delete succeeded on %s\n", objects.Location())
	return nil
}

// legacyPayloads reads captures straight from the bytea columns, so a dry run
// works against a database that predates migration 028.
type legacyPayloads struct {
	pool *pgstore.Pool
}

func (l legacyPayloads) ListConversationRequests(ctx context.Context, tenantID, conversationID string) ([]sessionarchive.PayloadRef, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT request_id, created_at FROM conversation_payloads
		 WHERE conversation_id = $2 AND COALESCE(tenant_id, '') = $1
		 ORDER BY created_at, request_id
	`, tenantID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []sessionarchive.PayloadRef
	for rows.Next() {
		var ref sessionarchive.PayloadRef
		if err := rows.Scan(&ref.RequestID, &ref.CreatedAt); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

func (l legacyPayloads) HasRequestsBefore(context.Context, string, string, time.Time, string) (bool, error) {
	return false, nil
}

func (l legacyPayloads) LoadPayload(ctx context.Context, requestID string) (*capture.Payload, error) {
	p := &capture.Payload{}
	var req, resp []byte
	err := l.pool.QueryRow(ctx, `
		SELECT request_id, COALESCE(conversation_id, ''), COALESCE(tenant_id, ''), COALESCE(user_id, ''), COALESCE(key_id, ''),
		       provider, model, status, COALESCE(error_class, ''), streaming,
		       request_body_compressed, response_body_compressed, request_truncated, response_truncated,
		       response_format, created_at, completed_at
		  FROM conversation_payloads WHERE request_id = $1
	`, requestID).Scan(&p.RequestID, &p.ConversationID, &p.TenantID, &p.UserID, &p.KeyID,
		&p.Provider, &p.Model, &p.Status, &p.ErrorClass, &p.Streaming,
		&req, &resp, &p.RequestTruncated, &p.ResponseTruncated,
		&p.ResponseFormat, &p.CreatedAt, &p.CompletedAt)
	if err != nil {
		return nil, fmt.Errorf("load capture %s: %w", requestID, err)
	}
	if p.RequestBody, err = capture.Decompress(req); err != nil {
		return nil, err
	}
	if p.ResponseBody, err = capture.Decompress(resp); err != nil {
		return nil, err
	}
	return p, nil
}

func (l legacyPayloads) ConversationBodyBytes(ctx context.Context, tenantID, conversationID string) (int64, error) {
	var total int64
	err := l.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(octet_length(request_body_compressed) + octet_length(response_body_compressed)), 0)
		  FROM conversation_payloads WHERE conversation_id = $2 AND COALESCE(tenant_id, '') = $1
	`, tenantID, conversationID).Scan(&total)
	return total, err
}

func formatKinds(kinds map[string]int) string {
	names := make([]string, 0, len(kinds))
	for name := range kinds {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = fmt.Sprintf("%s=%d", name, kinds[name])
	}
	return strings.Join(parts, " ")
}

func displayTenant(tenantID string) string {
	if tenantID == "" {
		return "(none)"
	}
	return tenantID
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
