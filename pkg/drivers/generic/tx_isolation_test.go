package generic

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
)

// recDriver records, in order, every connection open/close and every statement
// executed, so tests can assert that the isolation reset runs on the
// transaction's pinned connection before it goes back to the pool, and that a
// connection whose reset fails is discarded instead of reused.
type recDriver struct {
	mu       sync.Mutex
	log      []string
	opens    int
	failExec string // statements containing this substring fail
}

func (d *recDriver) record(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log = append(d.log, s)
}

func (d *recDriver) events() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.log...)
}

func (d *recDriver) Open(name string) (driver.Conn, error) {
	d.mu.Lock()
	d.opens++
	id := d.opens
	d.mu.Unlock()
	d.record("open")
	return &recConn{d: d, id: id}, nil
}

type recConn struct {
	d  *recDriver
	id int
}

func (c *recConn) Prepare(query string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *recConn) Close() error                              { c.d.record("close"); return nil }
func (c *recConn) Begin() (driver.Tx, error)                 { return c.BeginTx(context.Background(), driver.TxOptions{}) }

func (c *recConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.d.record("begin")
	return &recTx{d: c.d}, nil
}

func (c *recConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.d.failExec != "" && strings.Contains(query, c.d.failExec) {
		c.d.record("exec-fail:" + query)
		return nil, errors.New("exec failed")
	}
	c.d.record("exec:" + query)
	return driver.RowsAffected(0), nil
}

type recTx struct{ d *recDriver }

func (t *recTx) Commit() error   { t.d.record("commit"); return nil }
func (t *recTx) Rollback() error { t.d.record("rollback"); return nil }

func newRecGeneric(t *testing.T, name string, d *recDriver, resetSQL string) *Generic {
	t.Helper()
	sql.Register(name, d)
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("failed to close db: %v", err)
		}
	})
	return &Generic{DB: db, ResetIsolationSQL: resetSQL}
}

// After Commit, the reset statement must run on the transaction's connection
// before it is handed back to the pool.
func TestTxCommitRunsIsolationReset(t *testing.T) {
	d := &recDriver{}
	g := newRecGeneric(t, "rec-commit-reset", d, "RESET-ISOLATION")
	tx, err := g.BeginTx(context.Background(), &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	events := d.events()
	want := []string{"open", "begin", "commit", "exec:RESET-ISOLATION"}
	if len(events) != len(want) {
		t.Fatalf("expected %v, got %v", want, events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, events)
		}
	}
}

// Rollback must reset the connection too.
func TestTxRollbackRunsIsolationReset(t *testing.T) {
	d := &recDriver{}
	g := newRecGeneric(t, "rec-rollback-reset", d, "RESET-ISOLATION")
	tx, err := g.BeginTx(context.Background(), &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	events := d.events()
	want := []string{"open", "begin", "rollback", "exec:RESET-ISOLATION"}
	if len(events) != len(want) {
		t.Fatalf("expected %v, got %v", want, events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, events)
		}
	}
}

// The reset must run only once even when Commit is followed by the usual
// deferred Rollback.
func TestTxResetRunsOnceWithDeferredRollback(t *testing.T) {
	d := &recDriver{}
	g := newRecGeneric(t, "rec-once-reset", d, "RESET-ISOLATION")
	tx, err := g.BeginTx(context.Background(), &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != sql.ErrTxDone {
		t.Fatalf("expected sql.ErrTxDone, got %v", err)
	}
	resets := 0
	for _, e := range d.events() {
		if e == "exec:RESET-ISOLATION" {
			resets++
		}
	}
	if resets != 1 {
		t.Fatalf("expected exactly 1 reset, got %d (events: %v)", resets, d.events())
	}
}

// A connection whose reset fails must be discarded, not returned to the pool:
// the next query has to open a fresh connection.
func TestTxFailedResetDiscardsConnection(t *testing.T) {
	d := &recDriver{failExec: "RESET-ISOLATION"}
	g := newRecGeneric(t, "rec-failed-reset", d, "RESET-ISOLATION")
	tx, err := g.BeginTx(context.Background(), &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := g.DB.Exec("SELECT 1"); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	opens := d.opens
	d.mu.Unlock()
	if opens != 2 {
		t.Fatalf("expected the poisoned connection to be discarded and a fresh one opened (2 opens), got %d (events: %v)", opens, d.events())
	}
}

// Without ResetIsolationSQL the pinned connection is returned untouched and
// reused.
func TestTxNoResetReusesConnection(t *testing.T) {
	d := &recDriver{}
	g := newRecGeneric(t, "rec-no-reset", d, "")
	for i := 0; i < 2; i++ {
		tx, err := g.BeginTx(context.Background(), &sql.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	d.mu.Lock()
	opens := d.opens
	d.mu.Unlock()
	if opens != 1 {
		t.Fatalf("expected a single reused connection, got %d opens (events: %v)", opens, d.events())
	}
}
