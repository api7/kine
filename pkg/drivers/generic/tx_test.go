package generic

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"

	_ "github.com/mattn/go-sqlite3"
)

func TestTxRollbackAfterCommitIsNoop(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("failed to close db: %v", err)
		}
	})

	g := &Generic{DB: db}
	tx, err := g.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != sql.ErrTxDone {
		t.Fatalf("expected sql.ErrTxDone after commit, got %v", err)
	}
}

func TestTxRollbackAfterRollbackIsNoop(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("failed to close db: %v", err)
		}
	})

	g := &Generic{DB: db}
	tx, err := g.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != sql.ErrTxDone {
		t.Fatalf("expected sql.ErrTxDone after rollback, got %v", err)
	}
}

// abortedTxDriver simulates a server that has already terminated the
// transaction (e.g. MSSQL aborting a deadlock victim): the client-side
// ROLLBACK then fails with mssql error 3903.
type abortedTxDriver struct{}

func (d *abortedTxDriver) Open(name string) (driver.Conn, error) { return &abortedTxConn{}, nil }

type abortedTxConn struct{}

func (c *abortedTxConn) Prepare(query string) (driver.Stmt, error) {
	return nil, driver.ErrSkip
}
func (c *abortedTxConn) Close() error              { return nil }
func (c *abortedTxConn) Begin() (driver.Tx, error) { return &abortedTxTx{}, nil }

type abortedTxTx struct{}

func (t *abortedTxTx) Commit() error { return nil }
func (t *abortedTxTx) Rollback() error {
	return mssql.Error{Number: 3903, Message: "The ROLLBACK TRANSACTION request has no corresponding BEGIN TRANSACTION."}
}

func init() {
	sql.Register("aborted-tx", &abortedTxDriver{})
}

func TestTxRollbackAfterServerAbortIsNormalized(t *testing.T) {
	db, err := sql.Open("aborted-tx", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("failed to close db: %v", err)
		}
	})

	g := &Generic{DB: db, TxClosedErr: func(err error) bool {
		if mssqlErr, ok := err.(mssql.Error); ok {
			return mssqlErr.Number == 3903
		}
		return false
	}}
	tx, err := g.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != sql.ErrTxDone {
		t.Fatalf("expected sql.ErrTxDone when the server already terminated the transaction, got %v", err)
	}
	// MustRollback after the normalized rollback must be a no-op, not a fatal.
	tx.MustRollback()
}

func TestTxRollbackServerAbortWithoutHookSurfacesError(t *testing.T) {
	db, err := sql.Open("aborted-tx", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("failed to close db: %v", err)
		}
	})

	g := &Generic{DB: db}
	tx, err := g.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = tx.Rollback()
	if mssqlErr, ok := err.(mssql.Error); !ok || mssqlErr.Number != 3903 {
		t.Fatalf("expected the raw driver error without TxClosedErr, got %v", err)
	}
}
