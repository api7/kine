//go:build cgo

package endpoint

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type sqliteKine struct {
	client *clientv3.Client
	db     *sql.DB
	hook   *logtest.Hook
}

func startSQLiteKine(t *testing.T) *sqliteKine {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfg, err := Listen(ctx, Config{
		Endpoint: "sqlite://" + dbPath + "?_journal=WAL&cache=shared&_busy_timeout=30000",
		Listener: "unix://" + filepath.Join(dir, "kine.sock"),
	})
	if err != nil {
		t.Fatalf("start kine: %v", err)
	}

	client, err := clientv3.New(clientv3.Config{Endpoints: cfg.Endpoints, DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("connect to kine: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=30000")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	hook := logtest.NewGlobal()
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks)) })

	return &sqliteKine{client: client, db: db, hook: hook}
}

func (k *sqliteKine) put(t *testing.T, ctx context.Context, key, value string) int64 {
	t.Helper()
	resp, err := k.client.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, value)).
		Commit()
	if err != nil || !resp.Succeeded {
		t.Fatalf("create %s: succeeded=%v err=%v", key, resp != nil && resp.Succeeded, err)
	}
	return resp.Header.Revision
}

// compactTo records rev as the compact revision the same way the compactor's
// SetCompactRevision does. The compactor itself only runs on a five minute timer
// and always retains the latest 1000 revisions, so it cannot be driven from a test.
func (k *sqliteKine) compactTo(t *testing.T, rev int64) {
	t.Helper()
	if _, err := k.db.Exec(`UPDATE kine SET prev_revision = ? WHERE name = 'compact_rev_key'`, rev); err != nil {
		t.Fatalf("set compact revision: %v", err)
	}
}

// levelsOf returns the levels of the warning-or-worse entries whose message
// contains substr, so unrelated background logging does not affect assertions.
func (k *sqliteKine) levelsOf(substr string) []logrus.Level {
	var levels []logrus.Level
	for _, entry := range k.hook.AllEntries() {
		if entry.Level <= logrus.WarnLevel && strings.Contains(entry.Message, substr) {
			levels = append(levels, entry.Level)
		}
	}
	return levels
}

func expectLevels(t *testing.T, got []logrus.Level, want ...logrus.Level) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("expected log levels %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected log levels %v, got %v", want, got)
		}
	}
}

func TestWatchFromCompactedRevisionIsCanceledWithCompactRevisionAndLogsWarn(t *testing.T) {
	k := startSQLiteKine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	k.put(t, ctx, "/test/a", "1")
	compactRev := k.put(t, ctx, "/test/b", "2")
	k.put(t, ctx, "/test/c", "3")
	k.compactTo(t, compactRev)
	k.hook.Reset()

	// A watch includes its start revision, so starting at the compact revision
	// already asks for history that is gone.
	resp, ok := <-k.client.Watch(ctx, "/test/", clientv3.WithPrefix(), clientv3.WithRev(compactRev))
	if !ok {
		t.Fatal("watch channel closed without a cancel response")
	}
	if !resp.Canceled || !errors.Is(resp.Err(), rpctypes.ErrCompacted) {
		t.Fatalf("expected the watch to be canceled as compacted, got canceled=%v err=%v", resp.Canceled, resp.Err())
	}
	if resp.CompactRevision != compactRev {
		t.Fatalf("expected compact revision %d in the cancel response, got %d", compactRev, resp.CompactRevision)
	}
	expectLevels(t, k.levelsOf("Failed to list"), logrus.WarnLevel)

	// The client recovers by re-listing at the current revision, which succeeds.
	list, err := k.client.Get(ctx, "/test/", clientv3.WithPrefix())
	if err != nil || len(list.Kvs) != 3 {
		t.Fatalf("expected the re-list to return 3 keys, got %v (err %v)", list, err)
	}
}

func TestWatchListFailureLogsErrorAndEndsWatch(t *testing.T) {
	k := startSQLiteKine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rev := k.put(t, ctx, "/test/a", "1")
	if _, err := k.db.Exec(`ALTER TABLE kine RENAME TO kine_unavailable`); err != nil {
		t.Fatalf("break the kine table: %v", err)
	}
	k.hook.Reset()

	// The server cancels the watch without a compact revision, which the client
	// surfaces by closing the channel.
	for resp := range k.client.Watch(ctx, "/test/", clientv3.WithPrefix(), clientv3.WithRev(rev)) {
		if resp.CompactRevision != 0 || len(resp.Events) > 0 {
			t.Fatalf("expected the watch to end without events or a compact revision, got %+v", resp)
		}
	}
	if ctx.Err() != nil {
		t.Fatal("watch was not canceled by the server")
	}
	expectLevels(t, k.levelsOf("Failed to list"), logrus.ErrorLevel)
}

func TestRangeAtRevisionLogLevel(t *testing.T) {
	k := startSQLiteKine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	k.put(t, ctx, "/test/a", "1")
	compactRev := k.put(t, ctx, "/test/b", "2")
	currentRev := k.put(t, ctx, "/test/c", "3")
	k.compactTo(t, compactRev)

	for _, tc := range []struct {
		name    string
		opts    []clientv3.OpOption
		key     string
		rev     int64
		wantErr error
		want    []logrus.Level
	}{
		{name: "get below the compact revision is compacted and logs warn", key: "/test/a", rev: compactRev - 1, wantErr: rpctypes.ErrCompacted, want: []logrus.Level{logrus.WarnLevel}},
		{name: "list below the compact revision is compacted and logs warn", key: "/test/", opts: []clientv3.OpOption{clientv3.WithPrefix()}, rev: compactRev - 1, wantErr: rpctypes.ErrCompacted, want: []logrus.Level{logrus.WarnLevel}},
		{name: "list at the compact revision succeeds without logging", key: "/test/", opts: []clientv3.OpOption{clientv3.WithPrefix()}, rev: compactRev},
		{name: "list at a future revision fails and logs error", key: "/test/", opts: []clientv3.OpOption{clientv3.WithPrefix()}, rev: currentRev + 10, wantErr: rpctypes.ErrFutureRev, want: []logrus.Level{logrus.ErrorLevel}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k.hook.Reset()
			_, err := k.client.Get(ctx, tc.key, append(tc.opts, clientv3.WithRev(tc.rev))...)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v returned to the client, got %v", tc.wantErr, err)
			}
			expectLevels(t, k.levelsOf("error while range"), tc.want...)
		})
	}
}
