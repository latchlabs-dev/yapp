package memory

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/earendil-works/absurd/sdks/go/absurd"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestQueueDatabaseThroughPooler(t *testing.T) {
	dsn := os.Getenv("PIPROXY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PIPROXY_TEST_POSTGRES_DSN is not set")
	}
	app, db, err := newQueueClient(absurd.Options{DriverName: "pgx", DatabaseURL: dsn, QueueName: "pooler_review"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	defer app.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.CreateQueue(ctx, "pooler_review"); err != nil {
		t.Fatal(err)
	}
	spawned, err := app.Spawn(ctx, "review_task", map[string]string{"message": "pooler test"}, absurd.SpawnOptions{QueueName: "pooler_review"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.CancelTask(ctx, "pooler_review", spawned.TaskID); err != nil {
		t.Fatal(err)
	}
	if db.Stats().MaxOpenConnections != 2 {
		t.Fatal("queue pool is not bounded")
	}
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if db.Stats().Idle != 0 {
		t.Fatal("queue kept idle connections")
	}
}
