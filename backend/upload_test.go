package main

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"
)

// pipeSink is a controllable WriteCloser+WriterAt standing in for an SFTP
// file handle; WriteAt jitters so pipeline chunks genuinely complete out of
// order.
type pipeSink struct {
	mu     sync.Mutex
	buf    []byte
	closed bool
}

func (s *pipeSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	return len(p), nil
}

func (s *pipeSink) WriteAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	end := int(off) + len(p)
	if end > len(s.buf) {
		s.buf = append(s.buf, make([]byte, end-len(s.buf))...)
	}
	copy(s.buf[off:], p)
	time.Sleep(time.Duration(off%3) * time.Millisecond)
	return len(p), nil
}

func (s *pipeSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *pipeSink) segment(off, n int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]byte, n)
	copy(out, s.buf[off:off+n])
	return out
}

// TestUploadPipeline: chunks queued in arrival order must land at their
// offsets regardless of worker completion order, ack the monotonic written
// total, and finish (and deregister) the transfer once every byte is in.
func TestUploadPipeline(t *testing.T) {
	manager := NewManager(t.TempDir())
	sink := &pipeSink{}
	const chunks = 12
	const chunkSize = 1024
	transfer := &Transfer{
		ID:       "t1",
		Expected: chunks * chunkSize,
		sink:     sink,
		doneCh:   make(chan struct{}),
	}
	transfer.uploadJobs = make(chan uploadJob, downOutstanding)
	var writerAt io.WriterAt = sink
	for i := 0; i < downWorkers; i++ {
		go manager.uploadWorker(transfer, writerAt)
	}
	manager.mutex.Lock()
	manager.transfers[transfer.ID] = transfer
	manager.mutex.Unlock()

	patterns := make([][]byte, chunks)
	for i := 0; i < chunks; i++ {
		patterns[i] = bytes.Repeat([]byte{byte('a' + i)}, chunkSize)
		if err := manager.UploadChunk(transfer.ID, patterns[i]); err != nil {
			t.Fatalf("chunk %d rejected: %v", i, err)
		}
	}
	select {
	case <-transfer.doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline never finished")
	}
	manager.mutex.Lock()
	_, stillRegistered := manager.transfers[transfer.ID]
	manager.mutex.Unlock()
	if stillRegistered {
		t.Fatal("finished transfer was not deregistered")
	}
	if sink.writtenTotal() != chunks*chunkSize {
		t.Fatalf("written=%d want %d", sink.writtenTotal(), chunks*chunkSize)
	}
	for i := 0; i < chunks; i++ {
		if got := sink.segment(i*chunkSize, chunkSize); !bytes.Equal(got, patterns[i]) {
			t.Fatalf("chunk %d corrupted under out-of-order writes", i)
		}
	}
}

func (s *pipeSink) writtenTotal() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.buf))
}

// TestUploadChunkAfterFinish: a chunk queued for a transfer that finished
// mid-flight must fail loudly instead of wedging the handler.
func TestUploadChunkAfterFinish(t *testing.T) {
	manager := NewManager(t.TempDir())
	sink := &pipeSink{}
	transfer := &Transfer{ID: "t2", Expected: 8, sink: sink, doneCh: make(chan struct{})}
	transfer.uploadJobs = make(chan uploadJob, 1)
	manager.mutex.Lock()
	manager.transfers[transfer.ID] = transfer
	manager.mutex.Unlock()
	manager.finishTransfer(transfer.ID, nil)
	if err := manager.UploadChunk(transfer.ID, []byte("12345678")); err == nil {
		t.Fatal("chunk accepted for a finished transfer")
	}
}
