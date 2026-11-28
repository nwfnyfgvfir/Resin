package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Resinat/Resin/internal/routing"
)

type routedHTTPTransport struct {
	attempts           outboundAttempts
	transport          func(routedOutbound) *http.Transport
	health             HealthRecorder
	domain             string
	recordHTTPSLatency bool
	onRoute            func(routing.RouteResult)
	lifecycle          *requestLifecycle
}

func (t *routedHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	closeBody := req.Body != nil && req.Body != http.NoBody
	defer func() {
		if closeBody {
			_ = req.Body.Close()
		}
	}()

	feedback := attemptFeedback{attempts: t.attempts, health: t.health, lifecycle: t.lifecycle}
	replayable := !closeBody && req.Header.Get("Upgrade") == "" &&
		(req.Method == http.MethodGet || req.Method == http.MethodHead || req.Method == http.MethodOptions)
	lastErr := routing.ErrNoAvailableNodes
	for routed := range t.attempts.routes(ctx) {
		t.onRoute(routed.Route)
		start := time.Now()
		if t.lifecycle != nil {
			t.lifecycle.startRouteAttempt(routed.Route)
		}

		var waitingConn, started, responseStarted atomic.Bool
		attemptCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GetConn: func(string) { waitingConn.Store(true) },
			GotConn: func(httptrace.GotConnInfo) {
				started.Store(true)
				waitingConn.Store(false)
			},
			GotFirstResponseByte: func() {
				responseStarted.Store(true)
				if t.lifecycle != nil {
					t.lifecycle.markUpstreamFirstByte(start)
				}
			},
		})
		if t.recordHTTPSLatency && t.health != nil && req.URL.Scheme == "https" {
			reporter := newReverseLatencyReporter(t.health, routed.Route.NodeHash, t.domain)
			attemptCtx = httptrace.WithClientTrace(attemptCtx, reporter.clientTrace())
		}
		attempt := req.WithContext(attemptCtx)
		if closeBody {
			attempt.Body = &undialedRequestBody{ReadCloser: req.Body, started: &started}
			attempt.GetBody = nil
		}
		resp, err := t.transport(routed).RoundTrip(attempt)
		// Once a connection has been handed to net/http or it has read from
		// the body, the transport owns the stream and it may have sent data.
		if started.Load() {
			closeBody = false
		}
		if err == nil {
			feedback.confirm(routed.Route)
			feedback.finish(true)
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		var certErr *tls.CertificateVerificationError
		if errors.As(err, &certErr) {
			feedback.finish(false)
			return nil, err
		}
		beforeConn := waitingConn.Load() && !responseStarted.Load()
		earlyDisconnect := started.Load() && !responseStarted.Load() && isEarlyHTTPDisconnect(err)
		if beforeConn {
			feedback.failed(routed.Route, "http_connect", err, true)
		} else if earlyDisconnect {
			feedback.failed(routed.Route, "http_response_headers", err, true)
		}
		if !(beforeConn && !started.Load()) && !(replayable && (beforeConn || earlyDisconnect)) {
			feedback.finish(false)
			return nil, err
		}
		lastErr = err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	feedback.finish(false)
	return nil, lastErr
}

func isEarlyHTTPDisconnect(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE)
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
