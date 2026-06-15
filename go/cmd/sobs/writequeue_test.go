package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestWriteQueueWaitRunsOpSynchronously(t *testing.T) {
	q := newWriteQueue()
	var ran int32
	if err := q.enqueue(func() error { atomic.AddInt32(&ran, 1); return nil }, true); err != nil {
		t.Fatalf("enqueue(wait): %v", err)
	}
	if atomic.LoadInt32(&ran) != 1 {
		t.Error("wait=true should run the op before returning")
	}
}

func TestWriteQueueWaitPropagatesError(t *testing.T) {
	q := newWriteQueue()
	sentinel := errors.New("boom")
	if err := q.enqueue(func() error { return sentinel }, true); !errors.Is(err, sentinel) {
		t.Errorf("got %v, want the op error", err)
	}
}

func TestWriteQueueAsyncRuns(t *testing.T) {
	q := newWriteQueue()
	done := make(chan struct{})
	if err := q.enqueue(func() error { close(done); return nil }, false); err != nil {
		t.Fatalf("enqueue(async): %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("async op did not run")
	}
}

func TestWriteQueueBackpressure(t *testing.T) {
	// A queue with no worker draining it and a full channel -> next enqueue reports full.
	q := &writeQueue{ch: make(chan *writeTask, 1), batchMax: 1, batchWaitMs: 1}
	q.ch <- &writeTask{op: func() error { return nil }} // fill to capacity
	start := time.Now()
	err := q.enqueue(func() error { return nil }, false)
	if !errors.Is(err, errWriteQueueFull) {
		t.Errorf("got %v, want errWriteQueueFull", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("expected ~1s offer timeout, got %v", elapsed)
	}
}

func TestWriteQueueDepthNilSafe(t *testing.T) {
	var q *writeQueue
	if q.depth() != 0 {
		t.Error("nil queue depth should be 0")
	}
}

func TestWriteQueueBatchesMultiple(t *testing.T) {
	t.Setenv("SOBS_WRITE_BATCH_MAX", "8")
	t.Setenv("SOBS_WRITE_BATCH_WAIT_MS", "50")
	q := newWriteQueue()
	var ran int32
	for i := 0; i < 5; i++ {
		if err := q.enqueue(func() error { atomic.AddInt32(&ran, 1); return nil }, false); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	// Give the worker time to drain the batch.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&ran) < 5 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&ran); got != 5 {
		t.Errorf("ran %d ops, want 5", got)
	}
}
