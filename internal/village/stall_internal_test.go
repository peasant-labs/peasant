package village

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// trickleBody yields one byte per gap, modelling a slow upload link.
type trickleBody struct {
	remaining int
	gap       time.Duration
}

var _ io.Reader = (*trickleBody)(nil)

func (b *trickleBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	time.Sleep(b.gap)
	b.remaining--
	p[0] = 'x'
	return 1, nil
}

// serverDouble reads the whole request body, then waits headerDelay before it
// answers, and stops early when the request context ends.
type serverDouble struct {
	headerDelay time.Duration
	response    io.Reader
}

var _ http.RoundTripper = (*serverDouble)(nil)

func (s *serverDouble) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	buf := make([]byte, 64)
	for {
		_, err := req.Body.Read(buf)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	select {
	case <-time.After(s.headerDelay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(s.response), Header: make(http.Header)}, nil
}

func stallRequest(t *testing.T, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://village.example/upload", body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestStallTransportKeepsAProgressingUploadPastTheIdleDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const idle = time.Second
		transport := &stallTransport{base: &serverDouble{response: strings.NewReader("ok")}, idle: idle}
		start := time.Now()
		resp, err := transport.RoundTrip(stallRequest(t, &trickleBody{remaining: 200, gap: idle / 2}))
		if err != nil {
			t.Fatalf("progressing upload was cut: %v", err)
		}
		defer resp.Body.Close()
		if elapsed := time.Since(start); elapsed < 50*idle {
			t.Fatalf("upload took %v, want a transfer far longer than the idle deadline", elapsed)
		}
	})
}

func TestStallTransportCancelsAStalledUpload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const idle = time.Second
		transport := &stallTransport{base: &serverDouble{response: strings.NewReader("ok")}, idle: idle}
		_, err := transport.RoundTrip(stallRequest(t, &trickleBody{remaining: 3, gap: 2 * idle}))
		if !errors.Is(err, errTransferStalled) {
			t.Fatalf("err=%v, want errTransferStalled", err)
		}
		if !retryableTransportError(err) {
			t.Fatal("a stalled transfer must be retryable")
		}
	})
}

func TestStallTransportLeavesServerProcessingToTheHeaderDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const idle = time.Second
		transport := &stallTransport{base: &serverDouble{headerDelay: 5 * idle, response: strings.NewReader("ok")}, idle: idle}
		resp, err := transport.RoundTrip(stallRequest(t, strings.NewReader("payload")))
		if err != nil {
			t.Fatalf("server processing time was treated as a stall: %v", err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil || string(raw) != "ok" {
			t.Fatalf("body=%q err=%v", raw, err)
		}
	})
}

// blockingBody returns no bytes until its request context ends.
type blockingBody struct{ done <-chan struct{} }

var _ io.Reader = blockingBody{}

func (b blockingBody) Read([]byte) (int, error) {
	<-b.done
	return 0, errors.New("connection closed")
}

func TestStallTransportCancelsAStalledResponseBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const idle = time.Second
		base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(blockingBody{done: req.Context().Done()}), Header: make(http.Header)}, nil
		})
		transport := &stallTransport{base: base, idle: idle}
		resp, err := transport.RoundTrip(stallRequest(t, nil))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		if !errors.Is(err, errTransferStalled) {
			t.Fatalf("err=%v, want errTransferStalled", err)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

var _ http.RoundTripper = roundTripFunc(nil)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
