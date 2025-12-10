package mssql

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"

	"github.com/k3s-io/kine/pkg/drivers/generic"
	"github.com/k3s-io/kine/pkg/logstructured"
	"github.com/k3s-io/kine/pkg/logstructured/sqllog"
	"github.com/k3s-io/kine/pkg/server"
	"github.com/k3s-io/kine/pkg/tls"
	"github.com/k3s-io/kine/pkg/util"
)

const (
	defaultDSN = "sqlserver://sa@localhost:1433?database=kubernetes"
)

// MSSQL-specific SQL templates
// and doesn't allow ORDER BY in subqueries without TOP/OFFSET
var (
	columns = "kv.id AS theid, kv.name, kv.created, kv.deleted, kv.create_revision, kv.prev_revision, kv.lease, kv.value, kv.old_value"

	// MSSQL requires explicit column aliases for scalar subqueries in derived tables
	// The alias must be specified at the outer SELECT level, not inside the subquery
	revSQL = `SELECT MAX(rkv.id) FROM kine AS rkv`

	compactRevSQL = `SELECT MAX(crkv.prev_revision) FROM kine AS crkv WHERE crkv.name = 'compact_rev_key'`

	// The %s placeholder is for additional conditions like "AND mkv.id <= ?"
	// Parameter order matches generic.go: prefix, [revision], [startKey, revision], includeDeleted
	mssqlListSQL = fmt.Sprintf(`
		SELECT *
		FROM (
			SELECT (%s) AS id, (%s) AS compact_revision, %s
			FROM kine AS kv
			JOIN (
				SELECT MAX(mkv.id) AS id
				FROM kine AS mkv
				WHERE
					mkv.name LIKE ?
					%%s
				GROUP BY mkv.name) AS maxkv
				ON maxkv.id = kv.id
			WHERE
				? = 1 OR kv.deleted = 0
		) AS lkv
		ORDER BY lkv.theid ASC
		`, revSQL, compactRevSQL, columns)

	// For CountSQL, we need to remove ORDER BY from the subquery
	// MSSQL doesn't allow ORDER BY in subqueries unless TOP/OFFSET is used
	mssqlCountInnerSQL = fmt.Sprintf(`
		SELECT (%s) AS id, (%s) AS compact_revision, %s
		FROM kine AS kv
		JOIN (
			SELECT MAX(mkv.id) AS id
			FROM kine AS mkv
			WHERE
				mkv.name LIKE ?
			GROUP BY mkv.name) AS maxkv
			ON maxkv.id = kv.id
		WHERE
			? = 1 OR kv.deleted = 0
		`, revSQL, compactRevSQL, columns)

	mssqlIdOfKey = `
		AND
		mkv.id <= ? AND
		mkv.id > (
			SELECT MAX(ikv.id) AS id
			FROM kine AS ikv
			WHERE
				ikv.name = ? AND
				ikv.id <= ?)`
)

var (
	schema = []string{
		`IF NOT EXISTS (SELECT * FROM sysobjects WHERE name='kine' AND xtype='U')
		CREATE TABLE kine (
			id BIGINT IDENTITY(1,1) PRIMARY KEY,
			name NVARCHAR(630),
			created INT,
			deleted INT,
			create_revision BIGINT,
			prev_revision BIGINT,
			lease BIGINT,
			value VARBINARY(MAX),
			old_value VARBINARY(MAX)
		)`,
		`IF NOT EXISTS (SELECT * FROM sys.indexes WHERE name='kine_name_index' AND object_id = OBJECT_ID('kine'))
		CREATE INDEX kine_name_index ON kine (name)`,
		`IF NOT EXISTS (SELECT * FROM sys.indexes WHERE name='kine_name_id_index' AND object_id = OBJECT_ID('kine'))
		CREATE INDEX kine_name_id_index ON kine (name, id)`,
		`IF NOT EXISTS (SELECT * FROM sys.indexes WHERE name='kine_id_deleted_index' AND object_id = OBJECT_ID('kine'))
		CREATE INDEX kine_id_deleted_index ON kine (id, deleted)`,
		`IF NOT EXISTS (SELECT * FROM sys.indexes WHERE name='kine_prev_revision_index' AND object_id = OBJECT_ID('kine'))
		CREATE INDEX kine_prev_revision_index ON kine (prev_revision)`,
		`IF NOT EXISTS (SELECT * FROM sys.indexes WHERE name='kine_name_prev_revision_uindex' AND object_id = OBJECT_ID('kine'))
		CREATE UNIQUE INDEX kine_name_prev_revision_uindex ON kine (name, prev_revision)`,
	}
	createDB = "IF NOT EXISTS (SELECT * FROM sys.databases WHERE name = '%s') CREATE DATABASE [%s]"
)

// q replaces ? placeholders with numbered @p1, @p2... parameters for MSSQL
func q(sql string) string {
	n := 0
	result := make([]byte, 0, len(sql))
	for i := 0; i < len(sql); i++ {
		if sql[i] == '?' {
			n++
			result = append(result, "@p"...)
			result = append(result, strconv.Itoa(n)...)
		} else {
			result = append(result, sql[i])
		}
	}
	return string(result)
}

func New(ctx context.Context, dataSourceName string, tlsInfo tls.Config, connPoolConfig generic.ConnectionPoolConfig, metricsRegisterer prometheus.Registerer) (server.Backend, error) {
	parsedDSN, err := prepareDSN(dataSourceName, tlsInfo)
	if err != nil {
		return nil, err
	}

	if err := createDBIfNotExist(parsedDSN); err != nil {
		return nil, err
	}

	dialect, err := generic.Open(ctx, "sqlserver", parsedDSN, connPoolConfig, "@p", true, metricsRegisterer)
	if err != nil {
		return nil, err
	}

	dialect.LastInsertID = false

	// Override SQL statements with MSSQL-specific versions
	// MSSQL requires complete boolean expressions (1 = ? instead of just ?)
	// Use q() to convert ? to @p1, @p2... ensuring parameter order matches generic.go
	dialect.GetCurrentSQL = q(fmt.Sprintf(mssqlListSQL, ""))
	dialect.ListRevisionStartSQL = q(fmt.Sprintf(mssqlListSQL, "AND mkv.id <= ?"))
	dialect.GetRevisionAfterSQL = q(fmt.Sprintf(mssqlListSQL, mssqlIdOfKey))

	// CountSQL: MSSQL doesn't allow ORDER BY in subqueries without TOP/OFFSET
	// Also requires explicit column alias for the scalar subquery
	dialect.CountSQL = q(fmt.Sprintf(`
		SELECT (%s) AS id, COUNT(c.theid)
		FROM (
			%s
		) c`, revSQL, mssqlCountInnerSQL))

	dialect.AfterSQL = q(fmt.Sprintf(`
		SELECT (%s) AS id, (%s) AS compact_revision, %s
		FROM kine AS kv
		WHERE
			kv.name LIKE ? AND
			kv.id > ?
		ORDER BY kv.id ASC`, revSQL, compactRevSQL, columns))

	dialect.GetSizeSQL = `
		SELECT SUM(reserved_page_count) * 8 * 1024
		FROM sys.dm_db_partition_stats
		WHERE object_id = OBJECT_ID('kine')`
	dialect.CompactSQL = q(`
		DELETE kv FROM kine AS kv
		INNER JOIN (
			SELECT kp.prev_revision AS id
			FROM kine AS kp
			WHERE
				kp.name != 'compact_rev_key' AND
				kp.prev_revision != 0 AND
				kp.id <= ?
			UNION
			SELECT kd.id AS id
			FROM kine AS kd
			WHERE
				kd.deleted != 0 AND
				kd.id <= ?
		) AS ks
		ON kv.id = ks.id`)
	dialect.InsertSQL = q(`
		INSERT INTO kine(name, created, deleted, create_revision, prev_revision, lease, value, old_value)
		OUTPUT INSERTED.id
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`)
	dialect.FillSQL = q(`
		SET IDENTITY_INSERT kine ON;
		INSERT INTO kine(id, name, created, deleted, create_revision, prev_revision, lease, value, old_value)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?);
		SET IDENTITY_INSERT kine OFF`)
	dialect.FillRetryDuration = time.Millisecond * 5
	// MSSQL doesn't support DELETE FROM table AS alias syntax
	dialect.DeleteSQL = q(`DELETE FROM kine WHERE id = ?`)
	// MSSQL uses OFFSET...FETCH instead of LIMIT
	dialect.LimitSQL = func(sql string, limit int64) string {
		return fmt.Sprintf("%s OFFSET 0 ROWS FETCH NEXT %d ROWS ONLY", sql, limit)
	}
	dialect.InsertRetry = func(err error) bool {
		if mssqlErr, ok := err.(mssql.Error); ok {
			// 2627 = Violation of PRIMARY KEY constraint
			// 2601 = Cannot insert duplicate key row
			if mssqlErr.Number == 2627 || mssqlErr.Number == 2601 {
				return true
			}
		}
		return false
	}
	dialect.TranslateErr = func(err error) error {
		if mssqlErr, ok := err.(mssql.Error); ok {
			// 2627 = Violation of UNIQUE KEY constraint
			// 2601 = Cannot insert duplicate key row in object with unique index
			if mssqlErr.Number == 2627 || mssqlErr.Number == 2601 {
				return server.ErrKeyExists
			}
		}
		return err
	}
	dialect.ErrCode = func(err error) string {
		if err == nil {
			return ""
		}
		if mssqlErr, ok := err.(mssql.Error); ok {
			return strconv.Itoa(int(mssqlErr.Number))
		}
		return err.Error()
	}

	if err := setup(dialect.DB); err != nil {
		return nil, err
	}

	dialect.Migrate(context.Background())
	return logstructured.New(sqllog.New(dialect)), nil
}

func setup(db *sql.DB) error {
	logrus.Infof("Configuring database table schema and indexes, this may take a moment...")

	for _, stmt := range schema {
		logrus.Tracef("SETUP EXEC : %v", util.Stripped(stmt))
		_, err := db.Exec(stmt)
		if err != nil {
			return err
		}
	}

	logrus.Infof("Database tables and indexes are up to date")
	return nil
}

func createDBIfNotExist(dataSourceName string) error {
	u, err := url.Parse(dataSourceName)
	if err != nil {
		return err
	}

	query := u.Query()
	dbName := query.Get("database")
	if dbName == "" {
		dbName = "kubernetes"
	}

	// Connect to master database to create the target database
	query.Set("database", "master")
	u.RawQuery = query.Encode()

	db, err := sql.Open("sqlserver", u.String())
	if err != nil {
		logrus.Warnf("failed to ensure existence of database %s: unable to connect to master database: %v", dbName, err)
		return nil
	}
	defer db.Close()

	stmt := fmt.Sprintf(createDB, dbName, dbName)
	logrus.Tracef("SETUP EXEC : %v", util.Stripped(stmt))
	_, err = db.Exec(stmt)
	if err != nil {
		logrus.Warnf("failed to create database %s: %v", dbName, err)
	} else {
		logrus.Tracef("ensured database exists: %s", dbName)
	}
	return nil
}

func prepareDSN(dataSourceName string, tlsInfo tls.Config) (string, error) {
	if len(dataSourceName) == 0 {
		dataSourceName = defaultDSN
	} else {
		// Ensure the DSN has the sqlserver:// prefix
		if !strings.HasPrefix(dataSourceName, "sqlserver://") {
			dataSourceName = "sqlserver://" + dataSourceName
		}
	}

	u, err := url.Parse(dataSourceName)
	if err != nil {
		return "", err
	}

	query := u.Query()

	// Set default database if not specified
	if query.Get("database") == "" {
		query.Set("database", "kubernetes")
	}

	// Configure TLS if provided
	if tlsInfo.CertFile != "" || tlsInfo.KeyFile != "" || tlsInfo.CAFile != "" {
		// Enable encryption
		query.Set("encrypt", "true")

		if tlsInfo.CAFile != "" {
			query.Set("TrustServerCertificate", "false")
		} else {
			// If no CA file, trust the server certificate
			query.Set("TrustServerCertificate", "true")
		}
	}

	u.RawQuery = query.Encode()
	return u.String(), nil
}
