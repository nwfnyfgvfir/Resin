package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"

	"github.com/Resinat/Resin/internal/routing"
)

type routedHTTPTransport struct {
	attempts           outboundAttempts
	transport          func(routedOutbound) *http.Transport
	health             HealthRecorder
	domain             string
	recordHTTPSLatency bool
	onRoute            func(routing.RouteResult)
}

func (t *routedHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	closeBody := req.Body != nil && req.Body != http.NoBody
	defer func() {
		if closeBody {
			_ = req.Body.Close()
		}
	}()

	lastErr := routing.ErrNoAvailableNodes
	for routed := range t.attempts.routes(ctx) {
		t.onRoute(routed.Route)
		if t.health != nil {
			go t.health.RecordLatency(routed.Route.NodeHash, t.domain, nil)
		}

		var requestedConn, started atomic.Bool
		attemptCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GetConn: func(string) { requestedConn.Store(true) },
			GotConn: func(httptrace.GotConnInfo) { started.Store(true) },
		})
		if t.recordHTTPSLatency && t.health != nil && req.URL.Scheme == "https" {
			reporter := newReverseLatencyReporter(t.health, routed.Route.NodeHash, t.domain)
			attemptCtx = httptrace.WithClientTrace(attemptCtx, reporter.clientTrace())
		}
		attempt := req.WithContext(attemptCtx)
		if closeBody {
			attempt.Body = &undialedRequestBody{ReadCloser: req.Body, started: &started}
		}
		resp, err := t.transport(routed).RoundTrip(attempt)
		// Once a connection has been handed to net/http or it has read from
		// the body, the transport owns the stream and it may have sent data.
		if started.Load() {
			closeBody = false
		}
		if err == nil {
			t.attempts.router.CommitFailover(t.attempts.account, t.attempts.first, routed.Route)
			return resp, nil
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		recordPassiveResultAsync(t.health, routed.Route, false)
		if ctx.Err() != nil || started.Load() || !requestedConn.Load() {
			return nil, err
		}
		lastErr = err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, lastErr
}

// net/http closes the request body even when it fails to acquire a connection.
// Preserve the unread stream in that case so the next node can send it once.
type undialedRequestBody struct {
	io.ReadCloser
	started *atomic.Bool
}

func (b *undialedRequestBody) Read(p []byte) (int, error) {
	b.started.Store(true)
	return b.ReadCloser.Read(p)
}

func (b *undialedRequestBody) Close() error {
	if b.started.Load() {
		return b.ReadCloser.Close()
	}
	return nil
}
