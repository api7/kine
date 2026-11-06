package endpoint_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/k3s-io/kine/pkg/endpoint"
	"github.com/k3s-io/kine/pkg/logstructured"
)

// newClient starts kine on KINE_TEST_ENDPOINT (an empty SQLite database when
// unset) and returns a client connected to it, plus a key prefix unique to
// the test so runs against a shared database do not interfere.
func newClient(t *testing.T) (*clientv3.Client, string) {
	t.Helper()

	storage := os.Getenv("KINE_TEST_ENDPOINT")
	if storage == "" {
		storage = "sqlite://" + filepath.Join(t.TempDir(), "state.db") + "?_journal=WAL&cache=shared&_busy_timeout=30000"
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfg, err := endpoint.Listen(ctx, endpoint.Config{
		Listener:     "tcp://127.0.0.1:0",
		Endpoint:     storage,
		PollInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	cli, err := clientv3.New(clientv3.Config{Endpoints: cfg.Endpoints, DialTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	return cli, fmt.Sprintf("/txn-test/%s/%d/", t.Name(), time.Now().UnixNano())
}

func mustPut(t *testing.T, cli *clientv3.Client, key, value string) int64 {
	t.Helper()
	resp, err := cli.Txn(context.Background()).
		If(clientv3.Compare(clientv3.ModRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, value)).
		Commit()
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Succeeded {
		t.Fatalf("create %s did not succeed", key)
	}
	return resp.Header.Revision
}

func mustGet(t *testing.T, cli *clientv3.Client, key string) *mvccpb.KeyValue {
	t.Helper()
	resp, err := cli.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Kvs) == 0 {
		return nil
	}
	return resp.Kvs[0]
}

func TestMultiKeyTxnReplacesKeysAtomically(t *testing.T) {
	cli, prefix := newClient(t)
	ctx := context.Background()

	merged, a, b := prefix+"merged", prefix+"a", prefix+"b"
	mergedRev := mustPut(t, cli, merged, "merged")

	watch := cli.Watch(ctx, prefix, clientv3.WithPrefix(), clientv3.WithRev(mergedRev+1))

	txn := func() (*clientv3.TxnResponse, error) {
		return cli.Txn(ctx).
			If(
				clientv3.Compare(clientv3.ModRevision(merged), "=", mergedRev),
				clientv3.Compare(clientv3.CreateRevision(a), "=", 0),
				clientv3.Compare(clientv3.CreateRevision(b), "=", 0),
			).
			Then(
				clientv3.OpPut(a, "va"),
				clientv3.OpPut(b, "vb"),
				clientv3.OpDelete(merged, clientv3.WithPrevKV()),
			).
			Else(clientv3.OpGet(merged), clientv3.OpGet(a)).
			Commit()
	}

	resp, err := txn()
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Succeeded || len(resp.Responses) != 3 {
		t.Fatalf("unexpected response: %v", resp)
	}
	revA := resp.Responses[0].GetResponsePut().Header.Revision
	revB := resp.Responses[1].GetResponsePut().Header.Revision
	del := resp.Responses[2].GetResponseDeleteRange()
	if !(mergedRev < revA && revA < revB && revB < del.Header.Revision) || resp.Header.Revision != del.Header.Revision {
		t.Fatalf("unexpected revisions: merged=%d a=%d b=%d delete=%d header=%d", mergedRev, revA, revB, del.Header.Revision, resp.Header.Revision)
	}
	if del.Deleted != 1 || len(del.PrevKvs) != 1 || string(del.PrevKvs[0].Value) != "merged" {
		t.Fatalf("unexpected delete response: %v", del)
	}

	if kv := mustGet(t, cli, a); kv == nil || string(kv.Value) != "va" || kv.ModRevision != revA || kv.CreateRevision != revA {
		t.Fatalf("unexpected %s: %v", a, kv)
	}
	if kv := mustGet(t, cli, b); kv == nil || string(kv.Value) != "vb" || kv.ModRevision != revB {
		t.Fatalf("unexpected %s: %v", b, kv)
	}
	if kv := mustGet(t, cli, merged); kv != nil {
		t.Fatalf("%s still exists: %v", merged, kv)
	}

	var events []*clientv3.Event
	timeout := time.After(10 * time.Second)
	for len(events) < 3 {
		select {
		case wr := <-watch:
			if err := wr.Err(); err != nil {
				t.Fatal(err)
			}
			events = append(events, wr.Events...)
		case <-timeout:
			t.Fatalf("timed out waiting for watch events, got %d", len(events))
		}
	}
	want := []struct {
		typ mvccpb.Event_EventType
		key string
		rev int64
	}{
		{mvccpb.PUT, a, revA},
		{mvccpb.PUT, b, revB},
		{mvccpb.DELETE, merged, del.Header.Revision},
	}
	for i, w := range want {
		e := events[i]
		if e.Type != w.typ || string(e.Kv.Key) != w.key || e.Kv.ModRevision != w.rev {
			t.Fatalf("event %d: got %v %s@%d, want %v %s@%d", i, e.Type, e.Kv.Key, e.Kv.ModRevision, w.typ, w.key, w.rev)
		}
	}

	// The merged key is gone now, so the same guard fails and nothing is written.
	resp, err = txn()
	if err != nil {
		t.Fatal(err)
	}
	if resp.Succeeded || len(resp.Responses) != 2 {
		t.Fatalf("unexpected response: %v", resp)
	}
	if rng := resp.Responses[0].GetResponseRange(); rng.Count != 0 || len(rng.Kvs) != 0 {
		t.Fatalf("unexpected failure range for %s: %v", merged, rng)
	}
	if rng := resp.Responses[1].GetResponseRange(); rng.Count != 1 || string(rng.Kvs[0].Value) != "va" || rng.Kvs[0].ModRevision != revA {
		t.Fatalf("unexpected failure range for %s: %v", a, rng)
	}
	if resp.Header.Revision < del.Header.Revision {
		t.Fatalf("failed txn header revision %d is behind %d", resp.Header.Revision, del.Header.Revision)
	}
	if kv := mustGet(t, cli, a); kv.ModRevision != revA {
		t.Fatalf("%s was rewritten by a failed txn: %v", a, kv)
	}
}

func TestMultiKeyTxnUpdateAndRecreate(t *testing.T) {
	cli, prefix := newClient(t)
	ctx := context.Background()

	live, gone := prefix+"live", prefix+"gone"
	liveRev := mustPut(t, cli, live, "v1")
	mustPut(t, cli, gone, "old")
	if resp, err := cli.Txn(ctx).Then(clientv3.OpGet(gone), clientv3.OpDelete(gone)).Commit(); err != nil || !resp.Succeeded {
		t.Fatalf("delete %s: %v %v", gone, resp, err)
	}

	resp, err := cli.Txn(ctx).
		If(
			clientv3.Compare(clientv3.ModRevision(live), "=", liveRev),
			clientv3.Compare(clientv3.CreateRevision(gone), "=", 0),
		).
		Then(
			clientv3.OpPut(live, "v2"),
			clientv3.OpPut(gone, "new"),
			clientv3.OpDelete(prefix+"never-existed"),
		).
		Commit()
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Succeeded {
		t.Fatalf("unexpected response: %v", resp)
	}
	if d := resp.Responses[2].GetResponseDeleteRange(); d.Deleted != 0 {
		t.Fatalf("deleting a missing key reported %d deletions", d.Deleted)
	}

	kv := mustGet(t, cli, live)
	if string(kv.Value) != "v2" || kv.CreateRevision != liveRev || kv.ModRevision != resp.Responses[0].GetResponsePut().Header.Revision {
		t.Fatalf("unexpected %s: %v", live, kv)
	}
	kv = mustGet(t, cli, gone)
	if string(kv.Value) != "new" || kv.CreateRevision != kv.ModRevision {
		t.Fatalf("unexpected %s: %v", gone, kv)
	}
}

// Concurrent read-modify-write txns over the same two keys must serialize:
// every success increments both counters exactly once.
func TestMultiKeyTxnConcurrentCompareAndSwap(t *testing.T) {
	cli, prefix := newClient(t)
	ctx := context.Background()

	k1, k2 := prefix+"k1", prefix+"k2"
	mustPut(t, cli, k1, "0")
	mustPut(t, cli, k2, "0")

	const workers, increments = 8, 10
	var wg sync.WaitGroup
	errc := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for done := 0; done < increments; {
				g1, err := cli.Get(ctx, k1)
				if err != nil {
					errc <- err
					return
				}
				g2, err := cli.Get(ctx, k2)
				if err != nil {
					errc <- err
					return
				}
				n1, err := strconv.Atoi(string(g1.Kvs[0].Value))
				if err != nil {
					errc <- err
					return
				}
				n2, err := strconv.Atoi(string(g2.Kvs[0].Value))
				if err != nil {
					errc <- err
					return
				}
				put, err := cli.Txn(ctx).
					If(
						clientv3.Compare(clientv3.ModRevision(k1), "=", g1.Kvs[0].ModRevision),
						clientv3.Compare(clientv3.ModRevision(k2), "=", g2.Kvs[0].ModRevision),
					).
					Then(
						clientv3.OpPut(k1, strconv.Itoa(n1+1)),
						clientv3.OpPut(k2, strconv.Itoa(n2+1)),
					).
					Commit()
				if err != nil {
					errc <- err
					return
				}
				if put.Succeeded {
					done++
				}
			}
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}

	want := strconv.Itoa(workers * increments)
	if v := string(mustGet(t, cli, k1).Value); v != want {
		t.Fatalf("%s = %s, want %s", k1, v, want)
	}
	if v := string(mustGet(t, cli, k2).Value); v != want {
		t.Fatalf("%s = %s, want %s", k2, v, want)
	}
}

func TestMultiKeyTxnRejectsUnsupportedShapes(t *testing.T) {
	cli, prefix := newClient(t)
	ctx := context.Background()
	a, b := prefix+"a", prefix+"b"

	cases := map[string]clientv3.Txn{
		"range delete":  cli.Txn(ctx).Then(clientv3.OpPut(a, "1"), clientv3.OpDelete(prefix, clientv3.WithPrefix())),
		"nested txn":    cli.Txn(ctx).Then(clientv3.OpPut(a, "1"), clientv3.OpTxn(nil, []clientv3.Op{clientv3.OpPut(b, "1")}, nil)),
		"duplicate key": cli.Txn(ctx).Then(clientv3.OpPut(a, "1"), clientv3.OpDelete(a)),
		"compare on unwritten key": cli.Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(prefix+"other"), "=", 0)).
			Then(clientv3.OpPut(a, "1"), clientv3.OpPut(b, "1")),
		"compare on value": cli.Txn(ctx).
			If(clientv3.Compare(clientv3.Value(a), "=", "x")).
			Then(clientv3.OpPut(a, "1"), clientv3.OpPut(b, "1")),
		"put in failure branch": cli.Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(a), "=", 0)).
			Then(clientv3.OpPut(a, "1"), clientv3.OpPut(b, "1")).
			Else(clientv3.OpPut(b, "2")),
		"put with prev kv": cli.Txn(ctx).Then(clientv3.OpPut(a, "1", clientv3.WithPrevKV()), clientv3.OpPut(b, "1")),
	}
	for name, txn := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := txn.Commit()
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("got %v, want InvalidArgument", err)
			}
		})
	}

	if kv := mustGet(t, cli, a); kv != nil {
		t.Fatalf("rejected txns wrote %s: %v", a, kv)
	}
}

// interleaveCommit makes the next multi-key txn run commit right after it reads
// its first key, so the commit lands between that txn's per-key reads. It
// returns a function that reports whether commit ran.
func interleaveCommit(t *testing.T, commit func()) func() bool {
	t.Helper()
	var fired atomic.Bool
	logstructured.TxnReadHook = func(string) {
		if fired.CompareAndSwap(false, true) {
			commit()
		}
	}
	t.Cleanup(func() { logstructured.TxnReadHook = nil })
	return fired.Load
}

// The database only ever holds a=0,b=0 or a=1,b=1, so the failure reads must
// return one of those pairs, at a header revision that includes both.
func TestMultiKeyTxnFailureReadsOneSnapshot(t *testing.T) {
	cli, prefix := newClient(t)
	ctx := context.Background()

	a, b := prefix+"a", prefix+"b"
	revA, revB := mustPut(t, cli, a, "0"), mustPut(t, cli, b, "0")

	fired := interleaveCommit(t, func() {
		resp, err := cli.Txn(ctx).
			If(
				clientv3.Compare(clientv3.ModRevision(a), "=", revA),
				clientv3.Compare(clientv3.ModRevision(b), "=", revB),
			).
			Then(clientv3.OpPut(a, "1"), clientv3.OpPut(b, "1")).
			Commit()
		if err != nil || !resp.Succeeded {
			t.Errorf("concurrent commit: resp=%v err=%v", resp, err)
		}
	})

	resp, err := cli.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(a), "=", 0)).
		Then(clientv3.OpPut(a, "x"), clientv3.OpPut(b, "x")).
		Else(clientv3.OpGet(a), clientv3.OpGet(b)).
		Commit()
	if err != nil {
		t.Fatal(err)
	}
	if !fired() {
		t.Fatal("the concurrent commit did not run")
	}
	if resp.Succeeded || len(resp.Responses) != 2 {
		t.Fatalf("unexpected response: %v", resp)
	}
	gotA := resp.Responses[0].GetResponseRange().Kvs[0]
	gotB := resp.Responses[1].GetResponseRange().Kvs[0]
	if string(gotA.Value) != string(gotB.Value) {
		t.Fatalf("failure reads mix two states: %s=%s, %s=%s", a, gotA.Value, b, gotB.Value)
	}
	if resp.Header.Revision < gotA.ModRevision || resp.Header.Revision < gotB.ModRevision {
		t.Fatalf("header revision %d is older than the reads (%d, %d)", resp.Header.Revision, gotA.ModRevision, gotB.ModRevision)
	}
}

// The compares hold only on a mix of the state before and after the concurrent
// commit (a absent, b already updated), so the txn must fail.
func TestMultiKeyTxnComparesOneSnapshot(t *testing.T) {
	cli, prefix := newClient(t)
	ctx := context.Background()

	a, b := prefix+"a", prefix+"b"
	revB := mustPut(t, cli, b, "0")

	fired := interleaveCommit(t, func() {
		resp, err := cli.Txn(ctx).
			If(
				clientv3.Compare(clientv3.CreateRevision(a), "=", 0),
				clientv3.Compare(clientv3.ModRevision(b), "=", revB),
			).
			Then(clientv3.OpPut(a, "1"), clientv3.OpPut(b, "1")).
			Commit()
		if err != nil || !resp.Succeeded {
			t.Errorf("concurrent commit: resp=%v err=%v", resp, err)
		}
	})

	resp, err := cli.Txn(ctx).
		If(
			clientv3.Compare(clientv3.CreateRevision(a), "=", 0),
			clientv3.Compare(clientv3.ModRevision(b), ">", revB),
		).
		Then(clientv3.OpDelete(a), clientv3.OpPut(b, "x")).
		Commit()
	if err != nil {
		t.Fatal(err)
	}
	if !fired() {
		t.Fatal("the concurrent commit did not run")
	}
	if resp.Succeeded {
		t.Fatalf("compares passed on a mixed state: %v", resp)
	}
	if v := string(mustGet(t, cli, b).Value); v != "1" {
		t.Fatalf("%s = %s, want 1", b, v)
	}
}
