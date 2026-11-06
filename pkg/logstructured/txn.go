package logstructured

import (
	"context"
	"errors"

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
		latest, rev, err := readSnapshot(ctx, tx, ops)
		if err != nil {
			return err
		}
		res = server.TxnResult{
			Current:   make([]*server.KeyValue, len(ops)),
			Revisions: make([]int64, len(ops)),
			Revision:  rev,
		}
		for i, event := range latest {
			if event != nil && !event.Delete {
				res.Current[i] = event.KV
			}
		}

		if !guard(res.Current) {
			return nil
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
		return nil
	})
	if err != nil {
		return server.TxnResult{}, err
	}
	return res, nil
}

// errSnapshotChanged makes TxnLog.Txn run the transaction again.
var errSnapshotChanged = errors.New("keys changed while the txn read them")

// TxnReadHook, when set, is called after each key read in Txn. It is a test
// hook for interleaving concurrent writes and must stay nil otherwise.
var TxnReadHook func(key string)

// readSnapshot returns the latest event of every op key, all as of one moment,
// and a revision at that moment.
//
// The keys are read with separate statements, and a database at READ
// COMMITTED can commit between them. Pinning a revision and reading
// "id <= rev" does not help: ids are assigned at insert time, so a transaction
// holding lower ids can commit after the revision is read. Instead the keys are
// read twice. A key's latest event only ever moves forward, so a key that is
// the same in both passes was unchanged from its first read to its second,
// and every key was unchanged between the end of the first pass and the start
// of the second. The revision is read in that window.
func readSnapshot(ctx context.Context, tx LogTx, ops []server.TxnOp) ([]*server.Event, int64, error) {
	read := func() ([]*server.Event, error) {
		events := make([]*server.Event, len(ops))
		for i, op := range ops {
			event, err := tx.Latest(ctx, op.Key)
			if err != nil {
				return nil, err
			}
			events[i] = event
			if TxnReadHook != nil {
				TxnReadHook(op.Key)
			}
		}
		return events, nil
	}

	first, err := read()
	if err != nil {
		return nil, 0, err
	}
	rev, err := tx.CurrentRevision(ctx)
	if err != nil {
		return nil, 0, err
	}
	second, err := read()
	if err != nil {
		return nil, 0, err
	}
	for i := range first {
		if eventRevision(first[i]) != eventRevision(second[i]) {
			return nil, 0, errSnapshotChanged
		}
	}
	return first, rev, nil
}

func eventRevision(event *server.Event) int64 {
	if event == nil {
		return 0
	}
	return event.KV.ModRevision
}
