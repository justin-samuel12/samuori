package workeragent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
)

const (
	liveFlushEvery = 2 * time.Second
	liveChunk      = 64 << 10
	liveBuffer     = 2 << 20
)

// liveLog sends build output to the controller while the attempt runs (protocol 1.7). Writes never block the
// build: output beyond the buffer is dropped from the live view, and the final log is uploaded as before.
type liveLog struct {
	client workerv1.WorkerServiceClient
	lease  *workerv1.LeaseRef
	log    *slog.Logger

	mu      sync.Mutex
	pending []byte
	sent    uint64
	stopped bool
	done    chan struct{}
	exited  chan struct{}
}

func newLiveLog(ctx context.Context, client workerv1.WorkerServiceClient, lease *workerv1.LeaseRef, log *slog.Logger) *liveLog {
	l := &liveLog{client: client, lease: lease, log: log, done: make(chan struct{}), exited: make(chan struct{})}
	go l.run(ctx)
	return l
}

func (l *liveLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.stopped && len(l.pending)+len(p) <= liveBuffer {
		l.pending = append(l.pending, p...)
	}
	return len(p), nil
}

// Close sends what is buffered and stops.
func (l *liveLog) Close() {
	close(l.done)
	<-l.exited
}

func (l *liveLog) run(ctx context.Context) {
	defer close(l.exited)
	t := time.NewTicker(liveFlushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.done:
			l.flush(ctx)
			return
		case <-t.C:
			l.flush(ctx)
		}
	}
}

func (l *liveLog) flush(ctx context.Context) {
	for {
		l.mu.Lock()
		if l.stopped || len(l.pending) == 0 {
			l.mu.Unlock()
			return
		}
		chunk := l.pending[:min(len(l.pending), liveChunk)]
		offset := l.sent
		l.mu.Unlock()
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ack, err := l.client.AppendLog(cctx, &workerv1.LogChunk{Lease: l.lease, Offset: offset, Data: chunk})
		cancel()
		l.mu.Lock()
		if err != nil || ack.GetStop() {
			if err != nil {
				l.log.Debug("live log stopped", "attempt", l.lease.GetAttemptId(), "error", err)
			}
			l.stopped, l.pending = true, nil
			l.mu.Unlock()
			return
		}
		l.pending = l.pending[len(chunk):]
		l.sent += uint64(len(chunk))
		l.mu.Unlock()
	}
}
