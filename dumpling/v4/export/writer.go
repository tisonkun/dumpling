// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"text/template"

	"github.com/pingcap/br/pkg/storage"
	"github.com/pingcap/br/pkg/utils"
	"go.uber.org/zap"

	"github.com/pingcap/dumpling/v4/log"
)

type Writer struct {
	id         int64
	ctx        context.Context
	conf       *Config
	conn       *sql.Conn
	extStorage storage.ExternalStorage
	fileFmt    FileFormat

	receivedTaskCount int

	rebuildConnFn       func(*sql.Conn) (*sql.Conn, error)
	finishTaskCallBack  func(Task)
	finishTableCallBack func(Task)
}

func NewWriter(id int64, ctx context.Context, config *Config, conn *sql.Conn, externalStore storage.ExternalStorage) *Writer {
	sw := &Writer{
		id:                  id,
		ctx:                 ctx,
		conf:                config,
		conn:                conn,
		extStorage:          externalStore,
		finishTaskCallBack:  func(Task) {},
		finishTableCallBack: func(Task) {},
	}
	switch strings.ToLower(config.FileType) {
	case "sql":
		sw.fileFmt = FileFormatSQLText
	case "csv":
		sw.fileFmt = FileFormatCSV
	}
	return sw
}

func (w *Writer) setFinishTaskCallBack(fn func(Task)) {
	w.finishTaskCallBack = fn
}

func (w *Writer) setFinishTableCallBack(fn func(Task)) {
	w.finishTaskCallBack = fn
}

func countTotalTask(writers []*Writer) int {
	sum := 0
	for _, w := range writers {
		sum += w.receivedTaskCount
	}
	return sum
}

func (w *Writer) run(taskStream <-chan Task) error {
	for {
		select {
		case <-w.ctx.Done():
			log.Warn("context has been done, the writer will exit",
				zap.Int64("writer ID", w.id))
			return nil
		case task, ok := <-taskStream:
			if !ok {
				return nil
			}
			w.receivedTaskCount++
			err := w.handleTask(task)
			if err != nil {
				return err
			}
			w.finishTaskCallBack(task)
		}
	}
}

func (w *Writer) handleTask(task Task) error {
	switch t := task.(type) {
	case *TaskDatabaseMeta:
		return w.WriteDatabaseMeta(t.DatabaseName, t.CreateDatabaseSQL)
	case *TaskTableMeta:
		return w.WriteTableMeta(t.DatabaseName, t.TableName, t.CreateTableSQL)
	case *TaskViewMeta:
		return w.WriteViewMeta(t.DatabaseName, t.ViewName, t.CreateTableSQL, t.CreateViewSQL)
	case *TaskTableData:
		err := w.WriteTableData(t.Meta, t.Data, t.ChunkIndex)
		if err != nil {
			return err
		}
		if t.ChunkIndex+1 == t.TotalChunks {
			w.finishTableCallBack(task)
		}
		return nil
	default:
		log.Warn("unsupported writer task type", zap.String("type", fmt.Sprintf("%T", t)))
		return nil
	}
}

func (w *Writer) WriteDatabaseMeta(db, createSQL string) error {
	ctx, conf := w.ctx, w.conf
	fileName, err := (&outputFileNamer{DB: db}).render(conf.OutputFileTemplate, outputFileTemplateSchema)
	if err != nil {
		return err
	}
	return writeMetaToFile(ctx, db, createSQL, w.extStorage, fileName+".sql", conf.CompressType)
}

func (w *Writer) WriteTableMeta(db, table, createSQL string) error {
	ctx, conf := w.ctx, w.conf
	fileName, err := (&outputFileNamer{DB: db, Table: table}).render(conf.OutputFileTemplate, outputFileTemplateTable)
	if err != nil {
		return err
	}
	return writeMetaToFile(ctx, db, createSQL, w.extStorage, fileName+".sql", conf.CompressType)
}

func (w *Writer) WriteViewMeta(db, view, createTableSQL, createViewSQL string) error {
	ctx, conf := w.ctx, w.conf
	fileNameTable, err := (&outputFileNamer{DB: db, Table: view}).render(conf.OutputFileTemplate, outputFileTemplateTable)
	if err != nil {
		return err
	}
	fileNameView, err := (&outputFileNamer{DB: db, Table: view}).render(conf.OutputFileTemplate, outputFileTemplateView)
	if err != nil {
		return err
	}
	err = writeMetaToFile(ctx, db, createTableSQL, w.extStorage, fileNameTable+".sql", conf.CompressType)
	if err != nil {
		return err
	}
	return writeMetaToFile(ctx, db, createViewSQL, w.extStorage, fileNameView+".sql", conf.CompressType)
}

func (w *Writer) WriteTableData(meta TableMeta, ir TableDataIR, currentChunk int) error {
	ctx, conf, conn := w.ctx, w.conf, w.conn
	retryTime := 0
	var lastErr error
	return utils.WithRetry(ctx, func() (err error) {
		defer func() {
			lastErr = err
			if err != nil {
				errorCount.With(conf.Labels).Inc()
			}
		}()
		retryTime += 1
		log.Debug("trying to dump table chunk", zap.Int("retryTime", retryTime), zap.String("db", meta.DatabaseName()),
			zap.String("table", meta.TableName()), zap.Int("chunkIndex", currentChunk), zap.NamedError("lastError", lastErr))
		if retryTime > 1 {
			conn, err = w.rebuildConnFn(conn)
			if err != nil {
				return
			}
		}
		err = ir.Start(ctx, conn)
		if err != nil {
			return
		}
		defer ir.Close()
		return w.writeTableData(ctx, meta, ir, currentChunk)
	}, newDumpChunkBackoffer(canRebuildConn(conf.Consistency, conf.TransactionalConsistency)))
}

func (w *Writer) writeTableData(ctx context.Context, meta TableMeta, ir TableDataIR, curChkIdx int) error {
	conf, format := w.conf, w.fileFmt
	namer := newOutputFileNamer(meta, curChkIdx, conf.Rows != UnspecifiedSize, conf.FileSize != UnspecifiedSize)
	fileName, err := namer.NextName(conf.OutputFileTemplate, w.fileFmt.Extension())
	if err != nil {
		return err
	}

	for {
		fileWriter, tearDown := buildInterceptFileWriter(w.extStorage, fileName, conf.CompressType)
		err = format.WriteInsert(ctx, conf, meta, ir, fileWriter)
		tearDown(ctx)
		if err != nil {
			return err
		}

		if w, ok := fileWriter.(*InterceptFileWriter); ok && !w.SomethingIsWritten {
			break
		}

		if conf.FileSize == UnspecifiedSize {
			break
		}
		fileName, err = namer.NextName(conf.OutputFileTemplate, w.fileFmt.Extension())
		if err != nil {
			return err
		}
	}
	return nil
}

func writeMetaToFile(ctx context.Context, target, metaSQL string, s storage.ExternalStorage, path string, compressType storage.CompressType) error {
	fileWriter, tearDown, err := buildFileWriter(ctx, s, path, compressType)
	if err != nil {
		return err
	}
	defer tearDown(ctx)

	return WriteMeta(ctx, &metaData{
		target:  target,
		metaSQL: metaSQL,
		specCmts: []string{
			"/*!40101 SET NAMES binary*/;",
		},
	}, fileWriter)
}

type outputFileNamer struct {
	ChunkIndex int
	FileIndex  int
	DB         string
	Table      string
	format     string
}

type csvOption struct {
	nullValue string
	separator []byte
	delimiter []byte
}

func newOutputFileNamer(meta TableMeta, chunkIdx int, rows, fileSize bool) *outputFileNamer {
	o := &outputFileNamer{
		DB:    meta.DatabaseName(),
		Table: meta.TableName(),
	}
	o.ChunkIndex = chunkIdx
	o.FileIndex = 0
	if rows && fileSize {
		o.format = "%09d%04d"
	} else if fileSize {
		o.format = "%09[2]d"
	} else {
		o.format = "%09[1]d"
	}
	return o
}

func (namer *outputFileNamer) render(tmpl *template.Template, subName string) (string, error) {
	var bf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&bf, subName, namer); err != nil {
		return "", err
	}
	return bf.String(), nil
}

func (namer *outputFileNamer) Index() string {
	return fmt.Sprintf(namer.format, namer.ChunkIndex, namer.FileIndex)
}

func (namer *outputFileNamer) NextName(tmpl *template.Template, fileType string) (string, error) {
	res, err := namer.render(tmpl, outputFileTemplateData)
	namer.FileIndex++
	return res + "." + fileType, err
}
