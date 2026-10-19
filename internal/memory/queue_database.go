package memory

import (
	"database/sql"
	"time"

	"github.com/earendil-works/absurd/sdks/go/absurd"
)

// Queue operations are short database calls; LLM execution does not hold a
// connection. Each queue has two clients at most and keeps no idle connections.
// The queue owns this DB because Absurd does not close a caller-supplied DB.
func newQueueClient(opts absurd.Options) (*absurd.Client, *sql.DB, error) {
	db, err := sql.Open(opts.DriverName, opts.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(0)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(time.Minute)
	opts.DB = db
	app, err := absurd.New(opts)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return app, db, nil
}
