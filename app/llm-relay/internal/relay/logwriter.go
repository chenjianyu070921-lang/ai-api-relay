package relay

import (
	"context"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"gorm.io/gorm"

	"llm-relay/app/llm-relay/internal/model"
)

// LogWriter 异步日志写入：请求路径只投递，后台协程批量落库。
// 对齐 01-架构设计.md 第 2 节第 9 步。
type LogWriter struct {
	db        *gorm.DB
	ch        chan *model.RelayLog
	wg        sync.WaitGroup
	closeOnce sync.Once
}

func NewLogWriter(db *gorm.DB, buffer int) *LogWriter {
	if buffer <= 0 {
		buffer = 1024
	}
	w := &LogWriter{db: db, ch: make(chan *model.RelayLog, buffer)}
	if db != nil {
		w.wg.Add(1)
		go w.loop()
	}
	return w
}

// Write 非阻塞投递，缓冲满丢日志保主链路
func (w *LogWriter) Write(entry *model.RelayLog) {
	if w.db == nil || entry == nil {
		return
	}
	select {
	case w.ch <- entry:
	default:
		logx.Errorf("relay log channel full, dropping trace_id=%s", entry.TraceID)
	}
}

func (w *LogWriter) loop() {
	defer w.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	batch := make([]*model.RelayLog, 0, 128)
	for {
		select {
		case entry, ok := <-w.ch:
			if !ok {
				if len(batch) > 0 {
					w.flush(batch)
				}
				return
			}
			batch = append(batch, entry)
			if len(batch) >= 128 {
				w.flush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				w.flush(batch)
				batch = batch[:0]
			}
		}
	}
}

func (w *LogWriter) flush(batch []*model.RelayLog) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.db.WithContext(ctx).CreateInBatches(batch, 128).Error; err != nil {
		logx.Errorf("relay log flush failed: %v", err)
	}
}

// Close 优雅关闭
func (w *LogWriter) Close() {
	w.closeOnce.Do(func() {
		close(w.ch)
		w.wg.Wait()
	})
}
