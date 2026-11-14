package village

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Per-phase deadlines for the pooled client. They replace a single
// whole-exchange timeout, which cut a slow but progressing upload.
const (
	// DialTimeout bounds opening the TCP connection.
	DialTimeout = 10 * time.Second
	// TLSHandshakeTimeout bounds the TLS handshake.
	TLSHandshakeTimeout = 10 * time.Second
	// ResponseHeaderTimeout bounds the wait for response headers after the
	// request body has been fully written.
	ResponseHeaderTimeout = 60 * time.Second
	// TransferIdleTimeout bounds how long the request or response body may go
	// without moving a byte.
	TransferIdleTimeout = 30 * time.Second
)

var errTransferStalled = errors.New("village transfer stalled: no bytes moved within the idle deadline")

// stallTransport cancels a request whose body transfer stops making progress.
// The deadline restarts on every byte read from the request body and from the
// response body. It is paused between the end of the request body and the
// response headers, where ResponseHeaderTimeout applies instead.
type stallTransport struct {
	base http.RoundTripper
	idle time.Duration
}

var _ http.RoundTripper = (*stallTransport)(nil)

func (t *stallTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	watchdog := &stallWatchdog{idle: t.idle}
	watchdog.timer = time.AfterFunc(t.idle, func() { cancel(errTransferStalled) })
	out := req.Clone(ctx)
	if req.Body != nil && req.Body != http.NoBody {
		out.Body = &watchedBody{ReadCloser: req.Body, ctx: ctx, watchdog: watchdog, onEOF: watchdog.requestDone}
		if req.GetBody != nil {
			out.GetBody = func() (io.ReadCloser, error) {
				body, err := req.GetBody()
				if err != nil {
					return nil, err
				}
				return &watchedBody{ReadCloser: body, ctx: ctx, watchdog: watchdog, onEOF: watchdog.requestDone}, nil
			}
		}
	} else {
		watchdog.requestDone()
	}
	resp, err := t.base.RoundTrip(out)
	if err != nil {
		watchdog.stop()
		err = stallCause(ctx, err)
		cancel(nil)
		return nil, err
	}
	watchdog.responding()
	resp.Body = &watchedBody{ReadCloser: resp.Body, ctx: ctx, watchdog: watchdog, onClose: func() {
		watchdog.stop()
		cancel(nil)
	}}
	return resp, nil
}

func stallCause(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), errTransferStalled) && !errors.Is(err, errTransferStalled) {
		return fmt.Errorf("%w: %w", errTransferStalled, err)
	}
	return err
}

type stallWatchdog struct {
	mu       sync.Mutex
	idle     time.Duration
	timer    *time.Timer
	paused   bool
	response bool
	stopped  bool
}

func (w *stallWatchdog) progress() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.paused && !w.stopped {
		w.timer.Reset(w.idle)
	}
}

func (w *stallWatchdog) requestDone() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.response {
		w.paused = true
		w.timer.Stop()
	}
}

func (w *stallWatchdog) responding() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.response = true
	w.paused = false
	if !w.stopped {
		w.timer.Reset(w.idle)
	}
}

func (w *stallWatchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	w.timer.Stop()
}

type watchedBody struct {
	io.ReadCloser
	ctx      context.Context
	watchdog *stallWatchdog
	onEOF    func()
	onClose  func()
	once     sync.Once
}

var _ io.ReadCloser = (*watchedBody)(nil)

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.watchdog.progress()
	}
	if errors.Is(err, io.EOF) && b.onEOF != nil {
		b.onEOF()
	}
	if err != nil && !errors.Is(err, io.EOF) {
		err = stallCause(b.ctx, err)
	}
	return n, err
}

func (b *watchedBody) Close() error {
	err := b.ReadCloser.Close()
	if b.onClose != nil {
		b.once.Do(b.onClose)
	}
	return err
}
