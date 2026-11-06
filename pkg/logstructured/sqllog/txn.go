package sqllog

import (
	"context"
	"errors"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/k3s-io/kine/pkg/logstructured"
	"github.com/k3s-io/kine/pkg/server"
)

// explicit interface check
var _ logstructured.TxnLog = (*SQLLog)(nil)

const (
	txnAttempts     = 5
	txnRetryBackoff = 20 * time.Millisecond
)

// errTxnCommit marks a failed commit, which is not retried: the transaction
// may have been applied.
type errTxnCommit struct{ err error }

func (e *errTxnCommit) Error() string { return e.err.Error() }
func (e *errTxnCommit) Unwrap() error { return e.err }

// Txn runs fn in a database transaction. Concurrent writers to the same keys
// are detected by the (name, prev_revision) unique index, and a statement
// failure such as that conflict or a deadlock abort rolls the transaction back
// and runs fn again against fresh state.
func (s *SQLLog) Txn(ctx context.Context, fn func(tx logstructured.LogTx) error) error {
	for attempt := 1; ; attempt++ {
		rev, err := s.txnOnce(ctx, fn)
		if err == nil {
			if rev > 0 {
				select {
				case s.notify <- rev:
				default:
				}
			}
			return nil
		}

		var commitErr *errTxnCommit
		if attempt == txnAttempts || errors.As(err, &commitErr) || ctx.Err() != nil {
			return err
		}
		logrus.Debugf("TXN attempt %d failed, retrying: %v", attempt, err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * txnRetryBackoff):
		}
	}
}

func (s *SQLLog) txnOnce(ctx context.Context, fn func(tx logstructured.LogTx) error) (int64, error) {
	t, err := s.d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}

	tx := &sqlTx{t: t}
	if err := fn(tx); err != nil {
		if rbErr := t.Rollback(); rbErr != nil {
			logrus.Warnf("TXN rollback failed: %v", rbErr)
		}
		return 0, err
	}
	if err := t.Commit(); err != nil {
		return 0, &errTxnCommit{err: err}
	}
	return tx.last, nil
}

type sqlTx struct {
	t    server.Transaction
	last int64
}

func (x *sqlTx) Latest(ctx context.Context, key string) (*server.Event, error) {
	// key is a LIKE pattern, so it can match other keys too: keep the exact one.
	rows, err := x.t.ListCurrent(ctx, key, 0, true)
	if err != nil {
		return nil, err
	}
	_, _, events, err := RowsToEvents(rows)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if event.KV.Key == key {
			return event, nil
		}
	}
	return nil, nil
}

func (x *sqlTx) CurrentRevision(ctx context.Context) (int64, error) {
	return x.t.CurrentRevision(ctx)
}

func (x *sqlTx) Append(ctx context.Context, event *server.Event) (int64, error) {
	e := *event
	if e.KV == nil {
		e.KV = &server.KeyValue{}
	}
	if e.PrevKV == nil {
		e.PrevKV = &server.KeyValue{}
	}

	rev, err := x.t.Insert(ctx, e.KV.Key,
		e.Create,
		e.Delete,
		e.KV.CreateRevision,
		e.PrevKV.ModRevision,
		e.KV.Lease,
		e.KV.Value,
		e.PrevKV.Value,
	)
	if err != nil {
		return 0, err
	}
	if rev > x.last {
		x.last = rev
	}
	return rev, nil
}
