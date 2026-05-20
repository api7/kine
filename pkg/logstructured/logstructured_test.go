package logstructured

import (
	"context"
	"testing"
	"time"

	"github.com/k3s-io/kine/pkg/server"
)

type ttlWatchLog struct {
	listCalls     int
	afterRevision int64
	compactRev    int64
}

func (l *ttlWatchLog) Start(ctx context.Context) error {
	return nil
}

func (l *ttlWatchLog) CompactRevision(ctx context.Context) (int64, error) {
	return l.compactRev, nil
}

func (l *ttlWatchLog) CurrentRevision(ctx context.Context) (int64, error) {
	return 10, nil
}

func (l *ttlWatchLog) List(ctx context.Context, prefix, startKey string, limit, revision int64, includeDeletes bool) (int64, []*server.Event, error) {
	l.listCalls++
	if l.listCalls == 1 {
		return 10, []*server.Event{
			{
				KV: &server.KeyValue{
					Key:         "/old",
					ModRevision: 5,
					Lease:       60,
				},
			},
		}, nil
	}
	return 10, nil, nil
}

func (l *ttlWatchLog) After(ctx context.Context, prefix string, revision, limit int64) (int64, []*server.Event, error) {
	l.afterRevision = revision
	if revision < l.compactRev {
		return 10, nil, server.ErrCompacted
	}
	return 10, nil, nil
}

func (l *ttlWatchLog) Watch(ctx context.Context, prefix string) <-chan []*server.Event {
	ch := make(chan []*server.Event)
	close(ch)
	return ch
}

func (l *ttlWatchLog) Count(ctx context.Context, prefix string) (int64, int64, error) {
	return 0, 0, nil
}

func (l *ttlWatchLog) Append(ctx context.Context, event *server.Event) (int64, error) {
	return 0, nil
}

func (l *ttlWatchLog) DbSize(ctx context.Context) (int64, error) {
	return 0, nil
}

func TestTTLEventsWatchStartsAtCurrentListRevision(t *testing.T) {
	log := &ttlWatchLog{compactRev: 8}
	l := New(log)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	ch := l.ttlEvents(ctx)
	var events []*server.Event
	for {
		select {
		case event, ok := <-ch:
			if !ok {
				if len(events) != 1 {
					t.Fatalf("expected one TTL event from list, got %d", len(events))
				}
				if log.afterRevision != 9 {
					t.Fatalf("expected watch After revision 9, got %d", log.afterRevision)
				}
				return
			}
			events = append(events, event)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
