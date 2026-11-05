package logstructured

import (
	"context"

	"github.com/sirupsen/logrus"

	"github.com/k3s-io/kine/pkg/server"
)

// explicit interface check
var _ server.TxnBackend = (*LogStructured)(nil)

// TxnLog is implemented by logs that can run several reads and appends in one
// atomic transaction.
type TxnLog interface {
	// Txn runs fn in a transaction and commits it if fn returns nil. fn may be
	// called again after a conflicting concurrent write, so it must not keep
	// state across calls.
	Txn(ctx context.Context, fn func(tx LogTx) error) error
}

// LogTx is the view of the log inside a transaction.
type LogTx interface {
	// Latest returns the newest event for exactly key, including a deletion,
	// or nil if the key has never been written.
	Latest(ctx context.Context, key string) (*server.Event, error)
	CurrentRevision(ctx context.Context) (int64, error)
	Append(ctx context.Context, event *server.Event) (int64, error)
}

func (l *LogStructured) Txn(ctx context.Context, ops []server.TxnOp, guard func(current []*server.KeyValue) bool) (res server.TxnResult, errRet error) {
	defer func() {
		logrus.Tracef("TXN ops=%d => rev=%d, succeeded=%v, err=%v", len(ops), res.Revision, res.Succeeded, errRet)
	}()

	txnLog, ok := l.log.(TxnLog)
	if !ok {
		return res, server.ErrNotSupported
	}

	err := txnLog.Txn(ctx, func(tx LogTx) error {
		res = server.TxnResult{
			Current:   make([]*server.KeyValue, len(ops)),
			Revisions: make([]int64, len(ops)),
		}
		latest := make([]*server.Event, len(ops))
		for i, op := range ops {
			event, err := tx.Latest(ctx, op.Key)
			if err != nil {
				return err
			}
			latest[i] = event
			if event != nil && !event.Delete {
				res.Current[i] = event.KV
			}
		}

		if !guard(res.Current) {
			rev, err := tx.CurrentRevision(ctx)
			res.Revision = rev
			return err
		}

		// The events mirror the ones Create, Update and Delete append for a
		// single key.
		for i, op := range ops {
			var event *server.Event
			current := res.Current[i]
			switch {
			case op.Delete && current == nil:
				continue
			case op.Delete:
				event = &server.Event{
					Delete: true,
					KV:     current,
					PrevKV: current,
				}
			case current == nil:
				event = &server.Event{
					Create: true,
					KV: &server.KeyValue{
						Key:   op.Key,
						Value: op.Value,
						Lease: op.Lease,
					},
					PrevKV: &server.KeyValue{},
				}
				if latest[i] != nil {
					event.PrevKV = latest[i].KV
				}
			default:
				event = &server.Event{
					KV: &server.KeyValue{
						Key:            op.Key,
						CreateRevision: current.CreateRevision,
						Value:          op.Value,
						Lease:          op.Lease,
					},
					PrevKV: current,
				}
			}

			rev, err := tx.Append(ctx, event)
			if err != nil {
				return err
			}
			res.Revisions[i] = rev
			res.Revision = rev
		}

		res.Succeeded = true
		if res.Revision == 0 {
			rev, err := tx.CurrentRevision(ctx)
			res.Revision = rev
			return err
		}
		return nil
	})
	if err != nil {
		return server.TxnResult{}, err
	}
	return res, nil
}
