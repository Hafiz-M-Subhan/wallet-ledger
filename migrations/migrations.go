// Package migrations embeds the SQL schema and applies it at startup.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed *.sql
var files embed.FS

// advisoryLockID serialises schema application when several API instances
// start at the same time.
const advisoryLockID = 7_270_001

// Apply runs every embedded .sql file in name order. The files are written to
// be idempotent, so re-applying them on each start is safe.
func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockID); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", advisoryLockID) //nolint:errcheck

	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		sql, err := files.ReadFile(name)
		if err != nil {
			return err
		}
		// Simple-protocol exec so a file may hold many statements.
		if _, err := conn.Conn().PgConn().Exec(ctx, string(sql)).ReadAll(); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}
