// Package ctxio provides context-aware io constructs that handle cancellations during blocking read/write operations.
package ctxio

import (
	"context"
	"io"
	"sync"

	"golang.org/x/sync/errgroup"
)

type ReaderFanOut struct {
	Readers       []io.ReadCloser
	innerCloseErr error // guarded by sync.Once inside NewReader
	close         func(error) error
	wait          func() error
}

// Close closes the fan-out Readers as well as the underlying Reader if it implements io.Closer
func (m *ReaderFanOut) Close() error {
	// Stop goroutines
	m.close(nil)
	err := m.wait()
	// Close() should return any other error
	if err == io.ErrClosedPipe {
		// return the underlying reader's Close() error
		return m.innerCloseErr
	}
	return err
}

// NewReaderFanOut starts fan-out goroutines that duplicate the Reader r to n Readers
// Cancellation works even if the reader is blocked.
func NewReaderFanOut(ctx context.Context, r io.Reader, n int) *ReaderFanOut {
	ctx, cancel := context.WithCancel(ctx)
	g, gctx := errgroup.WithContext(ctx)

	m := &ReaderFanOut{
		Readers: make([]io.ReadCloser, n),
		wait:    g.Wait,
	}

	var once sync.Once
	var finalErr error

	pws := make([]*io.PipeWriter, n)
	ws := make([]io.Writer, n)
	for i := range n {
		m.Readers[i], pws[i] = io.Pipe()
		ws[i] = pws[i]
	}

	m.close = func(err error) error {
		once.Do(func() {
			if err != nil {
				println("TOTO", err.Error())
			}
			defer cancel()
			for _, pw := range pws {
				pw.CloseWithError(err)
			}
			if c, ok := r.(io.Closer); ok {
				m.innerCloseErr = c.Close()
				if err == nil {
					finalErr = m.innerCloseErr
					return
				}
			}
			finalErr = err
		})
		return finalErr
	}

	g.Go(func() error {
		defer m.close(nil) // cleanup upon panic
		_, err := io.Copy(io.MultiWriter(ws...), r)
		return m.close(err)
	})

	g.Go(func() error {
		defer m.close(nil) // cleanup upon panic
		<-gctx.Done()
		return m.close(gctx.Err())
	})

	return m
}

// NewReader returns a context-aware io.ReadCloser that reads from r.
// Cancellation works even if the reader is blocked.
func NewReader(ctx context.Context, r io.Reader) io.ReadCloser {
	out := NewReaderFanOut(ctx, r, 1)
	return struct {
		io.Reader
		io.Closer
	}{
		out.Readers[0],
		out,
	}
}
