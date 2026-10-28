package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// reconnecting hands database/sql a dead pooled PostgreSQL connection as driver.ErrBadConn when retrying is safe, so
// the pool retries on another connection instead of failing the request: BEGIN, and a SELECT outside a transaction.
// Writes outside a transaction and statements inside one are never retried
// (docs/operators/database.md#lost-connections).
type reconnecting struct{ driver.Connector }

func (c reconnecting) Connect(ctx context.Context) (driver.Conn, error) {
	dc, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &conn{Conn: dc.(*stdlib.Conn)}, nil
}

type conn struct {
	*stdlib.Conn
	inTx bool
}

func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.BeginTx(ctx, opts)
	if err != nil {
		return nil, c.badConn(ctx, err)
	}
	c.inTx = true
	return &txn{Tx: tx, c: c}, nil
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.QueryContext(ctx, query, args)
	if err != nil && !c.inTx && readOnly(query) {
		return nil, c.badConn(ctx, err)
	}
	return rows, err
}

func (c *conn) badConn(ctx context.Context, err error) error {
	if lost(ctx, err, c.Conn.Conn().IsClosed()) {
		return driver.ErrBadConn
	}
	return err
}

type txn struct {
	driver.Tx
	c *conn
}

func (t *txn) Commit() error {
	t.c.inTx = false
	return t.Tx.Commit()
}

func (t *txn) Rollback() error {
	t.c.inTx = false
	return t.Tx.Rollback()
}

// lost reports whether err means the server side of the connection is gone (a restart, failover or terminated
// backend) rather than a failed statement or a cancelled request.
func lost(ctx context.Context, err error, closed bool) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "57P01", "57P02", "57P03":
			return true
		}
		return strings.HasPrefix(pe.Code, "08")
	}
	return closed
}

func readOnly(query string) bool {
	q := strings.TrimLeftFunc(query, unicode.IsSpace)
	return len(q) > 6 && strings.EqualFold(q[:6], "SELECT") && unicode.IsSpace(rune(q[6]))
}
