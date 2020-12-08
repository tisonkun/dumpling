// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"context"
	"database/sql"

	"github.com/pingcap/br/pkg/utils"
	"github.com/pingcap/errors"
)

const (
	consistencyTypeAuto     = "auto"
	consistencyTypeFlush    = "flush"
	consistencyTypeLock     = "lock"
	consistencyTypeSnapshot = "snapshot"
	consistencyTypeNone     = "none"
)

// NewConsistencyController returns a new consistency controller
func NewConsistencyController(ctx context.Context, conf *Config, session *sql.DB) (ConsistencyController, error) {
	conn, err := session.Conn(ctx)
	if err != nil {
		return nil, errors.Trace(err)
	}
	switch conf.Consistency {
	case consistencyTypeFlush:
		return &ConsistencyFlushTableWithReadLock{
			serverType: conf.ServerInfo.ServerType,
			conn:       conn,
		}, nil
	case consistencyTypeLock:
		return &ConsistencyLockDumpingTables{
			conn:      conn,
			allTables: conf.Tables,
		}, nil
	case consistencyTypeSnapshot:
		if conf.ServerInfo.ServerType != ServerTypeTiDB {
			return nil, errors.New("snapshot consistency is not supported for this server")
		}
		return &ConsistencyNone{}, nil
	case consistencyTypeNone:
		return &ConsistencyNone{}, nil
	default:
		return nil, errors.Errorf("invalid consistency option %s", conf.Consistency)
	}
}

// ConsistencyController is the interface that controls the consistency of exporting progress
type ConsistencyController interface {
	Setup(context.Context) error
	TearDown(context.Context) error
	PingContext(context.Context) error
}

// ConsistencyNone dumps without adding locks, which cannot guarantee consistency
type ConsistencyNone struct{}

// Setup implements ConsistencyController.Setup
func (c *ConsistencyNone) Setup(_ context.Context) error {
	return nil
}

// TearDown implements ConsistencyController.TearDown
func (c *ConsistencyNone) TearDown(_ context.Context) error {
	return nil
}

// PingContext implements ConsistencyController.PingContext
func (c *ConsistencyNone) PingContext(_ context.Context) error {
	return nil
}

// ConsistencyFlushTableWithReadLock uses FlushTableWithReadLock before the dump
type ConsistencyFlushTableWithReadLock struct {
	serverType ServerType
	conn       *sql.Conn
}

// Setup implements ConsistencyController.Setup
func (c *ConsistencyFlushTableWithReadLock) Setup(ctx context.Context) error {
	if c.serverType == ServerTypeTiDB {
		return errors.New("'flush table with read lock' cannot be used to ensure the consistency in TiDB")
	}
	return FlushTableWithReadLock(ctx, c.conn)
}

// TearDown implements ConsistencyController.TearDown
func (c *ConsistencyFlushTableWithReadLock) TearDown(ctx context.Context) error {
	if c.conn == nil {
		return nil
	}
	defer func() {
		c.conn.Close()
		c.conn = nil
	}()
	return UnlockTables(ctx, c.conn)
}

// PingContext implements ConsistencyController.PingContext
func (c *ConsistencyFlushTableWithReadLock) PingContext(ctx context.Context) error {
	if c.conn == nil {
		return errors.New("consistency connection has already been closed")
	}
	return c.conn.PingContext(ctx)
}

// ConsistencyLockDumpingTables execute lock tables read on all tables before dump
type ConsistencyLockDumpingTables struct {
	conn      *sql.Conn
	allTables DatabaseTables
}

// Setup implements ConsistencyController.Setup
func (c *ConsistencyLockDumpingTables) Setup(ctx context.Context) error {
	blockList := make(map[string]map[string]interface{})
	return utils.WithRetry(ctx, func() error {
		lockTablesSQL := buildLockTablesSQL(c.allTables, blockList)
		_, err := c.conn.ExecContext(ctx, lockTablesSQL)
		return errors.Trace(err)
	}, newLockTablesBackoffer(blockList))
}

// TearDown implements ConsistencyController.TearDown
func (c *ConsistencyLockDumpingTables) TearDown(ctx context.Context) error {
	if c.conn == nil {
		return nil
	}
	defer func() {
		c.conn.Close()
		c.conn = nil
	}()
	return UnlockTables(ctx, c.conn)
}

// PingContext implements ConsistencyController.PingContext
func (c *ConsistencyLockDumpingTables) PingContext(ctx context.Context) error {
	if c.conn == nil {
		return errors.New("consistency connection has already been closed")
	}
	return c.conn.PingContext(ctx)
}

const snapshotFieldIndex = 1
