package generic

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestTxRollbackAfterCommitIsNoop(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

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
