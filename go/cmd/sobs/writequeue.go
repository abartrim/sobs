package main

import (
	"errors"
	"os"
	"strconv"
	"time"
)

// writeQueue is a faithful port of app.py's background DB-writer (_write_queue / _write_worker_main
// / _queue_write / _run_write_batch). chdb is single-process, so a single writer goroutine draining
// a bounded channel serializes all ingest writes and gives burst backpressure:
//
//   - SOBS_WRITE_QUEUE_MAX     (5000) — channel capacity; a full channel -> errWriteQueueFull (503).
//   - SOBS_WRITE_BATCH_MAX     (200)  — max tasks drained into one batch.
//   - SOBS_WRITE_BATCH_WAIT_MS (20)   — how long to keep filling a batch before running it.
//
// enqueue's `wait` mirrors Python's `wait = app.config["TESTING"]`: under the parity harness
// (SOBS_PARITY=1) writes are enqueued AND awaited (commit-before-ack), so the observable behaviour
// — and the golden corpus — is unchanged; in normal runtime writes are acked once queued.
type writeQueue struct {
	ch          chan *writeTask
	batchMax    int
	batchWaitMs int
}

type writeTask struct {
	op   func() error
	done chan struct{} // non-nil only for wait=true (synchronous) enqueues
	err  error
}

var errWriteQueueFull = errors.New("write queue is full")

func newWriteQueue() *writeQueue {
	q := &writeQueue{
		ch:          make(chan *writeTask, max(1, envInt("SOBS_WRITE_QUEUE_MAX", 5000))),
		batchMax:    max(1, envInt("SOBS_WRITE_BATCH_MAX", 200)),
		batchWaitMs: max(1, envInt("SOBS_WRITE_BATCH_WAIT_MS", 20)),
	}
	go q.worker()
	return q
}

// depth mirrors _write_queue_depth: the number of tasks waiting to be drained.
func (q *writeQueue) depth() int {
	if q == nil {
		return 0
	}
	return len(q.ch)
}

// enqueue mirrors _queue_write: offer the task within ~1s (else errWriteQueueFull). When wait, block
// on completion (15s cap, matching Python's best-effort done.wait) and return the op's error.
func (q *writeQueue) enqueue(op func() error, wait bool) error {
	t := &writeTask{op: op}
	if wait {
		t.done = make(chan struct{})
	}
	select {
	case q.ch <- t:
	case <-time.After(1 * time.Second):
		return errWriteQueueFull
	}
	if t.done != nil {
		select {
		case <-t.done:
		case <-time.After(15 * time.Second):
		}
		return t.err
	}
	return nil
}

// worker mirrors _write_worker_main: pull a first task, fill a batch up to batchMax or until the
// batchWaitMs deadline, then run it. Runs for the process lifetime (Python uses a daemon thread).
func (q *writeQueue) worker() {
	for first := range q.ch {
		batch := []*writeTask{first}
		timer := time.NewTimer(time.Duration(q.batchWaitMs) * time.Millisecond)
	fill:
		for len(batch) < q.batchMax {
			select {
			case t := <-q.ch:
				batch = append(batch, t)
			case <-timer.C:
				break fill
			}
		}
		timer.Stop()
		runWriteBatch(batch)
	}
}

// runWriteBatch mirrors _run_write_batch: run each op, record its error, signal waiters.
func runWriteBatch(batch []*writeTask) {
	for _, t := range batch {
		t.err = t.op()
		if t.done != nil {
			close(t.done)
		}
	}
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
