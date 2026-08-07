package generic

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"time"

	"github.com/k3s-io/kine/pkg/metrics"
	"github.com/k3s-io/kine/pkg/server"
	"github.com/k3s-io/kine/pkg/util"
	"github.com/sirupsen/logrus"
)

// explicit interface check
var _ server.Transaction = (*Tx)(nil)

type Tx struct {
	x      *sql.Tx
	conn   *sql.Conn
	d      *Generic
	closed bool
}

func (d *Generic) BeginTx(ctx context.Context, opts *sql.TxOptions) (server.Transaction, error) {
	logrus.Tracef("TX BEGIN")
	// Pin a dedicated connection instead of letting the pool pick one, so the
	// connection can have its session state reset before it is returned to the
	// pool. On SQL Server, SET TRANSACTION ISOLATION LEVEL (issued by BeginTx
	// for non-default isolation) is session-scoped and survives the
	// transaction: without a reset, a pooled connection that once ran a
	// SERIALIZABLE transaction keeps taking range locks for every later query.
	conn, err := d.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	x, err := conn.BeginTx(ctx, opts)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &Tx{
		x:    x,
		conn: conn,
		d:    d,
	}, nil
}

// releaseConn returns the transaction's pinned connection to the pool once the
// transaction has finished. If the dialect defines ResetIsolationSQL, it runs
// first; a connection whose reset fails is discarded rather than returned, so
// a session stuck at an elevated isolation level can never be reused.
func (t *Tx) releaseConn() {
	if t.conn == nil {
		return
	}
	conn := t.conn
	t.conn = nil
	if t.d.ResetIsolationSQL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(ctx, t.d.ResetIsolationSQL); err != nil {
			logrus.Warnf("TX release: isolation reset failed, discarding connection: %v", err)
			_ = conn.Raw(func(interface{}) error { return driver.ErrBadConn })
			_ = conn.Close()
			return
		}
	}
	_ = conn.Close()
}

func (t *Tx) Commit() error {
	logrus.Tracef("TX COMMIT")
	err := t.x.Commit()
	if err == nil {
		t.closed = true
	}
	// After Commit returns, the sql.Tx is finished regardless of the error, so
	// the pinned connection can always be released. Rollback stays callable
	// (it returns sql.ErrTxDone) and releaseConn is idempotent.
	t.releaseConn()
	return err
}

func (t *Tx) MustCommit() {
	if err := t.Commit(); err != nil {
		logrus.Fatalf("Transaction commit failed: %v", err)
	}
}

func (t *Tx) Rollback() error {
	logrus.Tracef("TX ROLLBACK")
	if t.closed {
		return sql.ErrTxDone
	}
	err := t.x.Rollback()
	if err != nil && err != sql.ErrTxDone && t.d.TxClosedErr != nil && t.d.TxClosedErr(err) {
		logrus.Warnf("TX ROLLBACK skipped, transaction already terminated by the server: %v", err)
		err = sql.ErrTxDone
	}
	if err == nil || err == sql.ErrTxDone {
		t.closed = true
		t.releaseConn()
	}
	return err
}

func (t *Tx) MustRollback() {
	if err := t.Rollback(); err != nil {
		if err != sql.ErrTxDone {
			logrus.Fatalf("Transaction rollback failed: %v", err)
		}
	}
}

func (t *Tx) GetCompactRevision(ctx context.Context) (int64, error) {
	var id int64
	row := t.queryRow(ctx, compactRevSQL)
	err := row.Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

func (t *Tx) SetCompactRevision(ctx context.Context, revision int64) error {
	logrus.Tracef("TX SETCOMPACTREVISION %v", revision)
	_, err := t.execute(ctx, t.d.UpdateCompactSQL, revision)
	return err
}

func (t *Tx) Compact(ctx context.Context, revision int64) (int64, error) {
	logrus.Tracef("TX COMPACT %v", revision)
	res, err := t.execute(ctx, t.d.CompactSQL, revision, revision)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (t *Tx) GetRevision(ctx context.Context, revision int64) (*sql.Rows, error) {
	return t.query(ctx, t.d.GetRevisionSQL, revision)
}

func (t *Tx) DeleteRevision(ctx context.Context, revision int64) error {
	logrus.Tracef("TX DELETEREVISION %v", revision)
	_, err := t.execute(ctx, t.d.DeleteSQL, revision)
	return err
}

func (t *Tx) CurrentRevision(ctx context.Context) (int64, error) {
	var id int64
	row := t.queryRow(ctx, revSQL)
	err := row.Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

func (t *Tx) query(ctx context.Context, sql string, args ...interface{}) (result *sql.Rows, err error) {
	logrus.Tracef("TX QUERY %v : %s", args, util.Stripped(sql))
	startTime := time.Now()
	defer func() {
		metrics.ObserveSQL(startTime, t.d.ErrCode(err), util.Stripped(sql), args)
	}()
	return t.x.QueryContext(ctx, sql, args...)
}

func (t *Tx) queryRow(ctx context.Context, sql string, args ...interface{}) (result *sql.Row) {
	logrus.Tracef("TX QUERY ROW %v : %s", args, util.Stripped(sql))
	startTime := time.Now()
	defer func() {
		metrics.ObserveSQL(startTime, t.d.ErrCode(result.Err()), util.Stripped(sql), args)
	}()
	return t.x.QueryRowContext(ctx, sql, args...)
}

func (t *Tx) execute(ctx context.Context, sql string, args ...interface{}) (result sql.Result, err error) {
	logrus.Tracef("TX EXEC %v : %s", args, util.Stripped(sql))
	startTime := time.Now()
	defer func() {
		metrics.ObserveSQL(startTime, t.d.ErrCode(err), util.Stripped(sql), args)
	}()
	return t.x.ExecContext(ctx, sql, args...)
}
