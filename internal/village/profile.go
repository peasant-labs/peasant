package village

import (
	"context"
	"io"
	"net/http"

	"github.com/peasant-labs/peasant/internal/perf"
)

type clientOperation string

const (
	operationPublish            clientOperation = "publish"
	operationVisibility         clientOperation = "visibility"
	operationAnnotations        clientOperation = "annotations"
	operationAnnotationManifest clientOperation = "annotation_manifest"
	operationNegotiate          clientOperation = "negotiate"
)

func (o clientOperation) stage() perf.StageID {
	switch o {
	case operationPublish:
		return perf.StagePushPublish
	case operationVisibility:
		return perf.StagePushVisibilityUpdate
	case operationNegotiate:
		return perf.StagePushNegotiate
	default:
		return perf.StagePushAnnotationsPublish
	}
}

type profileTransport struct {
	base      http.RoundTripper
	rec       perf.Recorder
	operation clientOperation
}

var _ http.RoundTripper = (*profileTransport)(nil)

func (t *profileTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	attrs := perf.Attributes{perf.AttrOperation: string(t.operation)}
	t.rec.Count(perf.CounterPushHTTPRequests, 1, perf.UnitRequests, attrs)
	t.rec.Count(perf.CounterPushHTTPResponses, 0, perf.UnitRequests, attrs)
	// VillageClient never automatically retries. Retry eligibility is not an
	// attempt; keep zero explicit rather than claiming an unobserved retry.
	t.rec.Count(perf.CounterPushHTTPRetries, 0, perf.UnitRequests, attrs)
	t.rec.Count(perf.CounterPushPayloadBytes, 0, perf.UnitBytes, attrs)
	t.rec.Count(perf.CounterPushResponseBytes, 0, perf.UnitBytes, attrs)
	if t.operation == operationVisibility {
		t.rec.Count(perf.CounterPushVisibilityPatchRequest, 1, perf.UnitRequests, attrs)
	}
	if t.operation == operationAnnotations || t.operation == operationAnnotationManifest {
		t.rec.Count(perf.CounterPushAnnotationRequests, 1, perf.UnitRequests, attrs)
	}
	if req.Body != nil {
		req = req.Clone(req.Context())
		req.Body = &profileBody{ReadCloser: req.Body, rec: t.rec, name: perf.CounterPushPayloadBytes, attrs: attrs}
	}
	resp, err := t.base.RoundTrip(req)
	status := "transport"
	if resp != nil {
		status = statusClass(resp.StatusCode)
		responseAttrs := perf.Attributes{perf.AttrOperation: string(t.operation), perf.AttrHTTPStatusClass: status}
		t.rec.Count(perf.CounterPushHTTPResponses, 1, perf.UnitRequests, responseAttrs)
		if resp.Body != nil {
			resp.Body = &profileBody{ReadCloser: resp.Body, rec: t.rec, name: perf.CounterPushResponseBytes, attrs: responseAttrs}
		}
	}
	if err != nil || (resp != nil && resp.StatusCode >= http.StatusBadRequest) {
		retryable := err != nil || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError
		code := "http_status"
		if err != nil {
			code = "transport"
		}
		t.rec.Error(t.operation.stage(), perf.SafeError{Code: code, Retryable: retryable, SafeMessage: "Village request failed; inspect command diagnostics and service availability before retrying"}, attrs)
		// This is the no-retry policy decision, not a fabricated timed attempt.
		t.rec.StartChildSpan(perf.StagePushRetry, perf.ParentSpanFromContext(req.Context()), perf.Attributes{perf.AttrOperation: string(t.operation), perf.AttrHTTPStatusClass: status}).End(perf.OutcomeSkipped, nil)
	}
	return resp, err
}

func statusClass(status int) string {
	switch {
	case status >= 100 && status < 200:
		return "1xx"
	case status < 300:
		return "2xx"
	case status < 400:
		return "3xx"
	case status < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

func profileResponseFailure(ctx context.Context, operation clientOperation) {
	perf.RecorderFromContext(ctx).Error(operation.stage(), perf.SafeError{Code: "response_decode", SafeMessage: "Village response could not be read or decoded; inspect command diagnostics and verify the service contract before retrying"}, perf.Attributes{perf.AttrOperation: string(operation)})
}

// Count bytes actually consumed by the transport/decoder, not Content-Length
// estimates. Read errors and close behavior pass through unchanged. No body,
// URL, header or filename enters profiling attributes.
type profileBody struct {
	io.ReadCloser
	rec   perf.Recorder
	name  perf.CounterName
	attrs perf.Attributes
}

var _ io.ReadCloser = (*profileBody)(nil)

func (b *profileBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.rec.Count(b.name, int64(n), perf.UnitBytes, b.attrs)
	}
	return n, err
}
