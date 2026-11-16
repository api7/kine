package server

import (
	"context"
	"errors"
	"testing"

	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
)

type rangeErrBackend struct {
	Backend
	err error
}

func (b *rangeErrBackend) Get(ctx context.Context, key, rangeEnd string, limit, revision int64) (int64, *KeyValue, error) {
	return 10, nil, b.err
}

func (b *rangeErrBackend) List(ctx context.Context, prefix, startKey string, limit, revision int64) (int64, []*KeyValue, error) {
	return 10, nil, b.err
}

func TestRangeLogLevel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rangeEnd []byte
		err      error
		want     logrus.Level
	}{
		{name: "get compacted", err: ErrCompacted, want: logrus.WarnLevel},
		{name: "list compacted", rangeEnd: []byte("/a0"), err: ErrCompacted, want: logrus.WarnLevel},
		{name: "get failure", err: errors.New("connection refused"), want: logrus.ErrorLevel},
		{name: "list failure", rangeEnd: []byte("/a0"), err: errors.New("connection refused"), want: logrus.ErrorLevel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook := logtest.NewGlobal()
			t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks)) })

			kv := New(&rangeErrBackend{err: tc.err}, "")
			_, err := kv.Range(context.Background(), &etcdserverpb.RangeRequest{Key: []byte("/a/"), RangeEnd: tc.rangeEnd, Revision: 5})
			if !errors.Is(err, tc.err) {
				t.Fatalf("expected %v returned to the client, got %v", tc.err, err)
			}

			var levels []logrus.Level
			for _, entry := range hook.AllEntries() {
				if entry.Level <= logrus.WarnLevel {
					levels = append(levels, entry.Level)
				}
			}
			if len(levels) != 1 || levels[0] != tc.want {
				t.Fatalf("expected a single %v entry, got %v", tc.want, levels)
			}
		})
	}
}
