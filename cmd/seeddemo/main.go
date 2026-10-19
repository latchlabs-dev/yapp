// Command seeddemo inserts synthetic tenants, users, and usage_events into the
// dev Postgres so the admin UI has something to render. TEMPORARY — delete the
// cmd/seeddemo directory after taking the preview. Run with:
//
//	PROXY_POSTGRES_DSN=postgres://proxy:proxy@localhost:5432/proxy?sslmode=disable go run ./cmd/seeddemo
package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/earlgrey/piproxy/internal/capture"
)

type tenant struct {
	id    string
	name  string
	users []user
}

type user struct {
	id    string
	email string
	name  string
}

func main() {
	dsn := os.Getenv("PROXY_POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://proxy:proxy@localhost:5432/proxy?sslmode=disable" //nolint:gosec // Local dev fallback DSN for this demo seeder only.
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	tenants := []tenant{
		{id: "ten_acme", name: "Acme AI", users: []user{
			{"usr_acme_jordan", "jordan@acme.ai", "Jordan Vega"},
			{"usr_acme_priya", "priya@acme.ai", "Priya Nair"},
		}},
		{id: "ten_globex", name: "Globex Labs", users: []user{
			{"usr_globex_sam", "sam@globex.dev", "Sam Okoro"},
			{"usr_globex_lin", "lin@globex.dev", "Lin Zhao"},
		}},
		{id: "ten_initech", name: "Initech", users: []user{
			{"usr_initech_dana", "dana@initech.io", "Dana Croft"},
		}},
	}

	// Clear prior demo rows so the seeder is re-runnable.
	if _, err := pool.Exec(ctx, `DELETE FROM usage_events WHERE request_id LIKE 'demo-%'`); err != nil {
		log.Fatalf("clear events: %v", err)
	}

	for _, t := range tenants {
		if _, err := pool.Exec(ctx,
			`INSERT INTO tenants (id, name, status) VALUES ($1,$2,'active')
			 ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name`, t.id, t.name); err != nil {
			log.Fatalf("tenant %s: %v", t.id, err)
		}
		for _, u := range t.users {
			if _, err := pool.Exec(ctx,
				`INSERT INTO users (id, tenant_id, email, display_name, status)
				 VALUES ($1,$2,$3,$4,'active')
				 ON CONFLICT (id) DO UPDATE SET display_name = EXCLUDED.display_name`,
				u.id, t.id, u.email, u.name); err != nil {
				log.Fatalf("user %s: %v", u.id, err)
			}
		}
	}

	type modelSpec struct {
		model, provider, upstream string
		inRate, outRate           float64 // cents per token
		weight                    float64
	}
	models := []modelSpec{
		{"claude-opus-4-8", "anthropic", "anthropic", 0.0015, 0.0075, 1.0},
		{"claude-sonnet-4-6", "anthropic", "anthropic", 0.0003, 0.0015, 1.3},
		{"gpt-5", "openai", "openai", 0.0010, 0.0030, 0.9},
		{"deepseek-v4-pro", "deepseek", "deepseek", 0.00014, 0.00028, 0.7},
		{"gemini-3-pro", "google", "google", 0.0007, 0.0021, 0.6},
		{"claude-haiku-4-5", "anthropic", "anthropic", 0.0001, 0.0005, 0.5},
	}
	ips := []string{"203.0.113.7", "198.51.100.23", "203.0.113.88", "192.0.2.44", "198.51.100.9"}

	now := time.Now().UTC()
	dayStart := func(d int) time.Time {
		t := now.AddDate(0, 0, -d)
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}

	n := 0
	days := 14
	for d := days - 1; d >= 0; d-- {
		// A rising wave with a weekly ripple so the trend lines have shape.
		phase := float64(days - 1 - d)
		wave := 0.55 + 0.45*math.Sin(phase/2.1) + phase*0.05
		base := dayStart(d)
		for ti, t := range tenants {
			tenantScale := []float64{1.0, 0.65, 0.4}[ti]
			for mi, m := range models {
				// Skip some tenant/model combos to make the mix uneven.
				if (ti+mi)%4 == 3 {
					continue
				}
				u := t.users[(d+mi)%len(t.users)]
				reqs := int(40 * wave * tenantScale * m.weight)
				if reqs < 1 {
					reqs = 1
				}
				inTok := int64(float64(reqs) * 850 * (0.8 + 0.4*math.Mod(float64(mi+1), 3)))
				outTok := int64(float64(inTok) * 0.4)
				total := inTok + outTok
				cost := float64(inTok)*m.inRate + float64(outTok)*m.outRate
				started := base.Add(time.Duration(9+mi) * time.Hour)
				completed := started.Add(3 * time.Second)
				id := fmt.Sprintf("demo-%04d", n)
				_, err := pool.Exec(ctx,
					`INSERT INTO usage_events
					 (id, request_id, tenant_id, user_id, provider, model,
					  upstream_provider, upstream_model, status,
					  input_tokens, output_tokens, total_tokens,
					  cost_total_cents, client_ip, started_at, completed_at)
					 VALUES ($1,$1,$2,$3,$4,$5,$4,$5,'success',
					         $6,$7,$8,$9,$10,$11,$12)`,
					id, t.id, u.id, m.provider, m.model,
					inTok, outTok, total, cost, ips[(n)%len(ips)], started, completed)
				if err != nil {
					log.Fatalf("event %s: %v", id, err)
				}
				n++
			}
		}
	}
	fmt.Printf("seeded %d usage_events across %d tenants over %d days\n", n, len(tenants), days)

	seedPayloads(ctx, pool, tenants, now)
	seedSessions(ctx, pool, now)
	seedMemoryRules(ctx, pool)
}

// gz compresses a JSON string using the same codec the proxy uses, so the
// admin API can decompress it on read.
func gz(s string) []byte {
	b, err := capture.Compress([]byte(s))
	if err != nil {
		log.Fatalf("compress: %v", err)
	}
	return b
}

func seedPayloads(ctx context.Context, pool *pgxpool.Pool, tenants []tenant, now time.Time) {
	if _, err := pool.Exec(ctx, `DELETE FROM conversation_payloads WHERE id LIKE 'demo-pl-%'`); err != nil {
		log.Fatalf("clear payloads: %v", err)
	}
	type turn struct{ role, text string }
	convos := []struct {
		conv   string
		model  string
		prov   string
		turns  []turn
		answer string
	}{
		{"conv-auth-refactor", "claude-opus-4-8", "anthropic", []turn{
			{"user", "Refactor the auth middleware to support API keys alongside bearer tokens."},
			{"assistant", "I'll add a key resolver that runs before the bearer check and short-circuits on a valid X-Admin-Key."},
			{"user", "Good. Now make sure revoked keys are rejected with 401, not 403."},
		}, "Done — revoked keys now return 401 with a WWW-Authenticate hint, and I added a table test covering active/revoked/expired."},
		{"conv-quota-bug", "claude-sonnet-4-6", "anthropic", []turn{
			{"user", "Users report quota resets at the wrong hour. Investigate the monthly rollover."},
			{"assistant", "The rollover used server-local midnight instead of UTC. Switching to UTC month boundaries."},
		}, "Fixed: month boundaries are computed in UTC. Added a regression test pinning a non-UTC TZ."},
		{"conv-charts", "gpt-5", "openai", []turn{
			{"user", "Redesign the usage charts to reduce clutter and add inline trend sparklines."},
			{"assistant", "Removing dashed grids, adding inline sparklines to the readout strip, and direct-labeling the model bars."},
		}, "Charts redesigned: data-ink maximized, range-framed cost line, endpoint readouts, sparklines per metric."},
	}
	count := 0
	for ci, c := range convos {
		// Two payload rows per conversation, a few hours apart.
		for rep := 0; rep < 2; rep++ {
			id := fmt.Sprintf("demo-pl-%02d", count)
			reqMsgs := ""
			for ti, t := range c.turns {
				if ti > 0 {
					reqMsgs += ","
				}
				reqMsgs += fmt.Sprintf(`{"role":%q,"content":%q}`, t.role, t.text)
			}
			reqBody := fmt.Sprintf(`{"model":%q,"max_tokens":2048,"messages":[%s]}`, c.model, reqMsgs)
			respBody := fmt.Sprintf(`{"id":"msg_%s_%d","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":%q}],"stop_reason":"end_turn","usage":{"input_tokens":%d,"output_tokens":%d}}`,
				c.conv, rep, c.model, c.answer, 1200+rep*300, 480+rep*120)
			created := now.Add(-time.Duration(ci*20+rep*6) * time.Hour)
			tn := tenants[ci%len(tenants)]
			usr := tn.users[0]
			_, err := pool.Exec(ctx,
				`INSERT INTO conversation_payloads
				 (id, request_id, conversation_id, tenant_id, user_id, provider, model,
				  status, streaming, request_body_compressed, response_body_compressed,
				  response_format, created_at, completed_at)
				 VALUES ($1,$1,$2,$3,$4,$5,$6,'success',$7,$8,$9,'json',$10,$10)`,
				id, c.conv, tn.id, usr.id, c.prov, c.model,
				rep == 1, gz(reqBody), gz(respBody), created)
			if err != nil {
				log.Fatalf("payload %s: %v", id, err)
			}
			count++
		}
	}
	fmt.Printf("seeded %d conversation_payloads across %d sessions\n", count, len(convos))
}

func seedSessions(ctx context.Context, pool *pgxpool.Pool, now time.Time) {
	if _, err := pool.Exec(ctx, `DELETE FROM session_submissions WHERE id LIKE 'demo-ss-%'`); err != nil {
		log.Fatalf("clear sessions: %v", err)
	}
	rows := []struct {
		conv      string
		project   string
		model     string
		msgs      int
		tokens    int64
		cost      float64
		distilled bool
		errMsg    string
	}{
		{"conv-auth-refactor", "project-alpha", "claude-opus-4-8", 42, 184320, 2.41, true, ""},
		{"conv-quota-bug", "project-alpha", "claude-sonnet-4-6", 28, 96400, 0.62, true, ""},
		{"conv-charts", "project-alpha", "gpt-5", 35, 142800, 1.18, false, ""},
		{"conv-data-sync", "project-beta", "claude-opus-4-8", 51, 233100, 3.07, true, ""},
		{"conv-importer", "project-gamma", "deepseek-v4-pro", 19, 71200, 0.11, false, "distiller: upstream timeout after 3 attempts"},
	}
	for i, r := range rows {
		id := fmt.Sprintf("demo-ss-%02d", i)
		started := now.Add(-time.Duration(i*9+5) * time.Hour)
		ended := started.Add(time.Duration(20+i*7) * time.Minute)
		summary := fmt.Sprintf(`{"conversation_id":%q,"project":%q,"turns":%d,"model":%q,"headline":"Worked on %s"}`,
			r.conv, r.project, r.msgs, r.model, r.project)
		var distilledAt *time.Time
		if r.distilled {
			d := ended.Add(2 * time.Minute)
			distilledAt = &d
		}
		attempts := 1
		if r.errMsg != "" {
			attempts = 3
		}
		_, err := pool.Exec(ctx,
			`INSERT INTO session_submissions
			 (id, conversation_id, session_file, message_count, model, total_tokens,
			  total_cost_cents, started_at, ended_at, summary_compressed,
			  extension_version, pi_version, project, distilled_at, distill_attempts, distill_error)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'0.4.2','1.9.0',$11,$12,$13,$14)`,
			id, r.conv, "~/.pi/sessions/"+r.conv+".json", r.msgs, r.model, r.tokens,
			r.cost, started, ended, gz(summary), r.project, distilledAt, attempts, nullStr(r.errMsg))
		if err != nil {
			log.Fatalf("session %s: %v", id, err)
		}
	}
	fmt.Printf("seeded %d session_submissions\n", len(rows))
}

func seedMemoryRules(ctx context.Context, pool *pgxpool.Pool) {
	if _, err := pool.Exec(ctx, `DELETE FROM memory_injection_rules WHERE id LIKE 'demo-mr-%'`); err != nil {
		log.Fatalf("clear rules: %v", err)
	}
	budget := func(n int) *int { return &n }
	rules := []struct {
		project string
		tenant  string
		user    string
		enabled bool
		budget  *int
		note    string
	}{
		{"", "", "", true, budget(4000), "Global default — inject distilled project memory into every context request."},
		{"project-alpha", "", "", true, budget(8000), "Larger budget for project-alpha; lots of architectural context."},
		{"project-gamma", "ten_acme", "", true, nil, "Acme's project-gamma work — tenant-scoped override."},
		{"project-beta", "", "", false, nil, "Paused while we re-distill project-beta's lessons."},
	}
	for i, r := range rules {
		id := fmt.Sprintf("demo-mr-%02d", i)
		_, err := pool.Exec(ctx,
			`INSERT INTO memory_injection_rules
			 (id, project, tenant_id, user_id, enabled, budget_tokens, note)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			id, nullStr(r.project), nullStr(r.tenant), nullStr(r.user), r.enabled, r.budget, nullStr(r.note))
		if err != nil {
			log.Fatalf("rule %s: %v", id, err)
		}
	}
	fmt.Printf("seeded %d memory_injection_rules\n", len(rules))
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
