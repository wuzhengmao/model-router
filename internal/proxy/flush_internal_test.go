package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

// flushRecorder counts explicit Flush calls so tests can distinguish buffered
// non-streaming writes from streaming ones.
type flushRecorder struct {
	header  http.Header
	body    bytes.Buffer
	flushes int
	writes  int
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{header: make(http.Header)}
}

func (f *flushRecorder) Header() http.Header { return f.header }

func (f *flushRecorder) Write(p []byte) (int, error) {
	f.writes++
	return f.body.Write(p)
}

func (f *flushRecorder) WriteHeader(int) {}

func (f *flushRecorder) Flush() { f.flushes++ }

// TestNonStreamingCopyDoesNotFlush covers §24.16 M15af: copyBody/copyBodyTee
// leave the response buffered so request statistics commit before the client
// sees EOF, even when the body spans several 32 KiB reads.
func TestNonStreamingCopyDoesNotFlush(t *testing.T) {
	payload := strings.Repeat("x", 3*32*1024+7)

	t.Run("copyBody", func(t *testing.T) {
		rec := newFlushRecorder()
		if err := copyBody(rec, strings.NewReader(payload)); err != nil {
			t.Fatalf("copyBody: %v", err)
		}
		if rec.flushes != 0 {
			t.Errorf("flushes = %d, want 0 (non-streaming responses stay buffered)", rec.flushes)
		}
		if rec.writes < 2 {
			t.Fatalf("writes = %d, want the body split across multiple reads", rec.writes)
		}
		if got := rec.body.String(); got != payload {
			t.Errorf("body length = %d, want %d", len(got), len(payload))
		}
	})

	t.Run("copyBodyTee", func(t *testing.T) {
		rec := newFlushRecorder()
		var capture bytes.Buffer
		if err := copyBodyTee(rec, strings.NewReader(payload), &capture); err != nil {
			t.Fatalf("copyBodyTee: %v", err)
		}
		if rec.flushes != 0 {
			t.Errorf("flushes = %d, want 0 (non-streaming responses stay buffered)", rec.flushes)
		}
		if rec.writes < 2 {
			t.Fatalf("writes = %d, want the body split across multiple reads", rec.writes)
		}
		if got := rec.body.String(); got != payload {
			t.Errorf("body length = %d, want %d", len(got), len(payload))
		}
		if got := capture.String(); got != payload {
			t.Errorf("captured length = %d, want %d", len(got), len(payload))
		}
	})
}

// TestStreamingCopyStillFlushes covers the other half of M15af: SSE bodies keep
// flushing every chunk.
func TestStreamingCopyStillFlushes(t *testing.T) {
	payload := strings.Repeat("data: {}\n\n", (3*32*1024)/9+1)
	rec := newFlushRecorder()
	body := io.NopCloser(strings.NewReader(payload))

	outcome, err := (&Handler{}).copySSE(rec, body, 0, 0, 0, false, false, func() {}, nil)
	if err != nil {
		t.Fatalf("copySSE: %v", err)
	}
	if outcome.chunks < 2 {
		t.Fatalf("chunks = %d, want multiple reads", outcome.chunks)
	}
	if rec.flushes != outcome.chunks {
		t.Errorf("flushes = %d, want one per chunk (%d)", rec.flushes, outcome.chunks)
	}
	if rec.body.String() != payload {
		t.Errorf("streamed body length = %d, want %d", rec.body.Len(), len(payload))
	}
}
