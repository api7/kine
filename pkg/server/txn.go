package server

import (
	"context"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxTxnOps matches etcd's default --max-txn-ops.
const maxTxnOps = 128

// TxnOp is a single-key write applied by a multi-key transaction.
type TxnOp struct {
	Key    string
	Value  []byte
	Lease  int64
	Delete bool
}

// TxnResult is the outcome of TxnBackend.Txn.
type TxnResult struct {
	// Revision is the revision of the last applied op, or the current
	// revision when nothing was written.
	Revision  int64
	Succeeded bool
	// Current holds, for each op, the key's value before the transaction,
	// or nil if the key did not exist.
	Current []*KeyValue
	// Revisions holds, for each op, the revision it was written at, or zero
	// if the op wrote nothing (deleting a key that does not exist).
	Revisions []int64
}

// TxnBackend is implemented by backends that can apply writes to several keys
// in one atomic transaction.
type TxnBackend interface {
	// Txn reads the current value of every op key and passes them to guard.
	// If guard returns true, the ops are applied in order, each at its own
	// revision; either all of them are applied or none.
	Txn(ctx context.Context, ops []TxnOp, guard func(current []*KeyValue) bool) (TxnResult, error)
}

type multiKeyTxn struct {
	ops      []TxnOp
	compares []*etcdserverpb.Compare
	prevKV   []bool
	index    map[string]int
	// failure holds, for each failure range request, the index of the op
	// whose key it reads.
	failure []int
}

func unsupportedTxn(reason string) error {
	return status.New(codes.InvalidArgument, "etcdserver: unsupported operations in txn request: "+reason).Err()
}

// parseMultiKeyTxn accepts a transaction whose success branch is a list of
// single-key puts and deletes on distinct keys, guarded by mod or create
// revision comparisons on those keys, with an optional failure branch of
// single-key reads of those keys.
func parseMultiKeyTxn(txn *etcdserverpb.TxnRequest) (*multiKeyTxn, error) {
	if len(txn.Success) == 0 {
		return nil, unsupportedTxn("empty success branch")
	}
	if len(txn.Success) > maxTxnOps {
		return nil, unsupportedTxn("too many operations")
	}

	m := &multiKeyTxn{
		ops:      make([]TxnOp, 0, len(txn.Success)),
		prevKV:   make([]bool, 0, len(txn.Success)),
		compares: txn.Compare,
		index:    make(map[string]int, len(txn.Success)),
	}
	index := m.index

	for _, op := range txn.Success {
		var (
			txnOp  TxnOp
			prevKV bool
		)
		switch {
		case op.GetRequestPut() != nil:
			put := op.GetRequestPut()
			if put.IgnoreLease || put.IgnoreValue || put.PrevKv {
				return nil, unsupportedTxn("put with ignore_lease, ignore_value or prev_kv")
			}
			txnOp = TxnOp{Key: string(put.Key), Value: put.Value, Lease: put.Lease}
		case op.GetRequestDeleteRange() != nil:
			del := op.GetRequestDeleteRange()
			if len(del.RangeEnd) != 0 {
				return nil, unsupportedTxn("range delete")
			}
			txnOp = TxnOp{Key: string(del.Key), Delete: true}
			prevKV = del.PrevKv
		case op.GetRequestTxn() != nil:
			return nil, unsupportedTxn("nested txn")
		default:
			return nil, unsupportedTxn("success branch may only contain put and delete")
		}
		if txnOp.Key == "" {
			return nil, unsupportedTxn("empty key")
		}
		if _, ok := index[txnOp.Key]; ok {
			return nil, unsupportedTxn("duplicate key " + txnOp.Key)
		}
		index[txnOp.Key] = len(m.ops)
		m.ops = append(m.ops, txnOp)
		m.prevKV = append(m.prevKV, prevKV)
	}

	for _, c := range txn.Compare {
		if len(c.RangeEnd) != 0 {
			return nil, unsupportedTxn("compare on a key range")
		}
		if c.Target != etcdserverpb.Compare_MOD && c.Target != etcdserverpb.Compare_CREATE {
			return nil, unsupportedTxn("compare target other than mod or create revision")
		}
		if _, ok := index[string(c.Key)]; !ok {
			return nil, unsupportedTxn("compare on a key the txn does not write")
		}
	}

	for _, op := range txn.Failure {
		rng := op.GetRequestRange()
		if rng == nil {
			return nil, unsupportedTxn("failure branch may only contain range")
		}
		if len(rng.RangeEnd) != 0 || rng.Revision != 0 || rng.KeysOnly || rng.CountOnly ||
			rng.SortOrder != 0 || rng.SortTarget != 0 ||
			rng.MinModRevision != 0 || rng.MaxModRevision != 0 ||
			rng.MinCreateRevision != 0 || rng.MaxCreateRevision != 0 {
			return nil, unsupportedTxn("failure range other than a plain single-key get")
		}
		i, ok := index[string(rng.Key)]
		if !ok {
			return nil, unsupportedTxn("failure range on a key the txn does not write")
		}
		m.failure = append(m.failure, i)
	}

	return m, nil
}

func (m *multiKeyTxn) guard(current []*KeyValue) bool {
	for _, c := range m.compares {
		kv := current[m.index[string(c.Key)]]
		var actual, expected int64
		if kv != nil {
			if c.Target == etcdserverpb.Compare_MOD {
				actual = kv.ModRevision
			} else {
				actual = kv.CreateRevision
			}
		}
		if c.Target == etcdserverpb.Compare_MOD {
			expected = c.GetModRevision()
		} else {
			expected = c.GetCreateRevision()
		}

		var ok bool
		switch c.Result {
		case etcdserverpb.Compare_EQUAL:
			ok = actual == expected
		case etcdserverpb.Compare_NOT_EQUAL:
			ok = actual != expected
		case etcdserverpb.Compare_GREATER:
			ok = actual > expected
		case etcdserverpb.Compare_LESS:
			ok = actual < expected
		}
		if !ok {
			return false
		}
	}
	return true
}

func (l *LimitedServer) multiKeyTxn(ctx context.Context, txn *etcdserverpb.TxnRequest) (*etcdserverpb.TxnResponse, error) {
	backend, ok := l.backend.(TxnBackend)
	if !ok {
		return nil, ErrNotSupported
	}

	m, err := parseMultiKeyTxn(txn)
	if err != nil {
		return nil, err
	}

	res, err := backend.Txn(ctx, m.ops, m.guard)
	if err != nil {
		return nil, err
	}

	resp := &etcdserverpb.TxnResponse{
		Header:    txnHeader(res.Revision),
		Succeeded: res.Succeeded,
	}

	if !res.Succeeded {
		for _, i := range m.failure {
			rng := &etcdserverpb.RangeResponse{Header: txnHeader(res.Revision)}
			if kv := res.Current[i]; kv != nil {
				rng.Kvs = toKVs(kv)
				rng.Count = 1
			}
			resp.Responses = append(resp.Responses, &etcdserverpb.ResponseOp{
				Response: &etcdserverpb.ResponseOp_ResponseRange{ResponseRange: rng},
			})
		}
		return resp, nil
	}

	for i, op := range m.ops {
		rev := res.Revisions[i]
		if rev == 0 {
			rev = res.Revision
		}
		if !op.Delete {
			resp.Responses = append(resp.Responses, &etcdserverpb.ResponseOp{
				Response: &etcdserverpb.ResponseOp_ResponsePut{
					ResponsePut: &etcdserverpb.PutResponse{Header: txnHeader(rev)},
				},
			})
			continue
		}
		del := &etcdserverpb.DeleteRangeResponse{Header: txnHeader(rev)}
		if kv := res.Current[i]; kv != nil {
			del.Deleted = 1
			if m.prevKV[i] {
				del.PrevKvs = toKVs(kv)
			}
		}
		resp.Responses = append(resp.Responses, &etcdserverpb.ResponseOp{
			Response: &etcdserverpb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: del},
		})
	}
	return resp, nil
}
