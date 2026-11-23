package store

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// Berth shares its PostgreSQL server with the apps it manages; it must not hold many connections.
func TestPoolIsCapped(t *testing.T) {
	dsn := os.Getenv("BERTH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BERTH_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn+"&search_path=store_test")
	if err != nil {
		// the schema may not exist yet; create it through a plain connection first
		raw, e := Open(ctx, dsn)
		if e != nil {
			t.Fatal(e)
		}
		raw.Pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS store_test`)
		raw.Pool.Close()
		if st, err = Open(ctx, dsn+"&search_path=store_test"); err != nil {
			t.Fatal(err)
		}
	}
	defer st.Pool.Close()
	if got := st.Pool.Config().MaxConns; got != maxPoolConns || got > 5 {
		t.Fatalf("pool max is %d, want %d", got, maxPoolConns)
	}
	// 40 concurrent queries must never open more than the cap
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st.Pool.Exec(ctx, `SELECT pg_sleep(0.05)`)
		}()
	}
	time.Sleep(60 * time.Millisecond)
	if n := st.Pool.Stat().TotalConns(); n > maxPoolConns {
		t.Fatalf("pool opened %d connections, cap is %d", n, maxPoolConns)
	}
	wg.Wait()
}
