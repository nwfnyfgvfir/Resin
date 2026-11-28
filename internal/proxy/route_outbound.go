package proxy

import (
	"context"
	"fmt"
	"iter"
	"net"
	"sync/atomic"
	"time"

	"github.com/Resinat/Resin/internal/outbound"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

type routedOutbound struct {
	Route        routing.RouteResult
	Outbound     adapter.Outbound
	stalledDials *atomic.Int32
}

func dialOutbound(ctx context.Context, timeout time.Duration, ob adapter.Outbound, stalled *atomic.Int32, network string, addr M.Socksaddr) (net.Conn, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if stalled != nil && stalled.Load() >= maxStalledOutboundDials {
		return nil, fmt.Errorf("previous outbound dials are still blocked: %w", context.DeadlineExceeded)
	}
	// Some outbounds use the context for their whole stream. Stop the dial
	// timer at connection acquisition and cancel the context on Close.
	ctx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(timeout, func() { cancel(context.DeadlineExceeded) })
	defer timer.Stop()
	type result struct {
		conn net.Conn
		err  error
	}
	completed := make(chan result)
	abandoned := make(chan struct{})
	go func() {
		conn, err := ob.DialContext(ctx, network, addr)
		select {
		case completed <- result{conn: conn, err: err}:
		case <-abandoned:
			if conn != nil {
				_ = conn.Close()
			}
			if stalled != nil {
				stalled.Add(-1)
			}
		}
	}()
	select {
	case res := <-completed:
		if !timer.Stop() {
			cancel(context.DeadlineExceeded)
		}
		if cause := context.Cause(ctx); cause != nil {
			res.err = cause
		}
		if res.err == nil {
			return &outboundDialConn{Conn: res.conn, cancel: cancel}, nil
		}
		cancel(res.err)
		if res.conn != nil {
			_ = res.conn.Close()
		}
		return nil, res.err
	case <-ctx.Done():
		// Some adapters also block on their own connection setup. Keep the
		// caller's budget and drain any connection that eventually arrives.
		if stalled != nil {
			stalled.Add(1)
		}
		close(abandoned)
		return nil, context.Cause(ctx)
	}
}

type outboundDialConn struct {
	net.Conn
	cancel context.CancelCauseFunc
}

func (c *outboundDialConn) Close() error {
	c.cancel(nil)
	return c.Conn.Close()
}

func (c *outboundDialConn) CloseWrite() error { return closeWriteErr(c.Conn) }
func (c *outboundDialConn) CloseRead() error  { return closeReadErr(c.Conn) }

const (
	maxOutboundAttempts     = 3
	outboundDialTimeout     = 5 * time.Second
	maxStalledOutboundDials = 4
)

type outboundAttempts struct {
	router  *routing.Router
	pool    outbound.PoolAccessor
	first   routing.RouteResult
	account string
	target  string
}

func selectOutboundAttempts(
	router *routing.Router,
	pool outbound.PoolAccessor,
	platformName string,
	account string,
	target string,
) (outboundAttempts, *ProxyError) {
	result, err := router.RouteRequest(platformName, account, target)
	if err != nil {
		return outboundAttempts{}, mapRouteError(err)
	}
	return outboundAttempts{router: router, pool: pool, first: result, account: account, target: target}, nil
}

func (a outboundAttempts) routes(ctx context.Context) iter.Seq[routedOutbound] {
	return func(yield func(routedOutbound) bool) {
		if ctx.Err() != nil {
			return
		}
		n := 0
		if first, ok := a.outbound(a.first); ok {
			n++
			if !yield(first) {
				return
			}
		}
		for route := range a.router.AlternateRoutes(ctx, a.account, a.first, a.target) {
			if next, ok := a.outbound(route); ok {
				n++
				if !yield(next) || n == maxOutboundAttempts {
					return
				}
			}
		}
	}
}

func (a outboundAttempts) outbound(result routing.RouteResult) (routedOutbound, bool) {
	entry, ok := a.pool.GetEntry(result.NodeHash)
	if !ok || !entry.IsHealthy() || entry.GetEgressIP() != result.EgressIP {
		return routedOutbound{}, false
	}
	obPtr := entry.Outbound.Load()
	if obPtr == nil {
		return routedOutbound{}, false
	}

	return routedOutbound{
		Route:        result,
		Outbound:     *obPtr,
		stalledDials: &entry.ProxyDialsStalled,
	}, true
}

// attemptFeedback keeps account recovery separate from global health feedback.
// A sequence of connection attempts contributes at most one passive failure.
type attemptFeedback struct {
	attempts      outboundAttempts
	health        HealthRecorder
	lifecycle     *requestLifecycle
	firstFailure  routing.RouteResult
	failedBinding routing.RouteResult
	multipleExits bool
}

func (f *attemptFeedback) failed(route routing.RouteResult, stage string, err error, affectsLease bool) {
	if f.lifecycle != nil {
		f.lifecycle.recordFirstAttemptFailure(route, stage, err)
	}
	if f.firstFailure.PlatformID == "" {
		f.firstFailure = route
	} else if f.firstFailure.EgressIP != route.EgressIP {
		f.multipleExits = true
	}
	if affectsLease && route.HasLeaseBinding() {
		f.failedBinding = route
	}
}

func (f *attemptFeedback) confirm(route routing.RouteResult) {
	observed := f.attempts.first
	if route.HasLeaseBinding() {
		observed = route
	} else if f.failedBinding.HasLeaseBinding() {
		observed = f.failedBinding
	}
	f.attempts.router.CommitFailover(f.attempts.account, observed, route)
}

func (f *attemptFeedback) finish(connected bool) {
	if f.firstFailure.PlatformID == "" {
		return
	}
	if !connected {
		f.attempts.router.NoteLeaseFailure(f.attempts.account, f.failedBinding)
	}
	// Failures on several exits without a working route are ambiguous.
	if connected || !f.multipleExits {
		recordPassiveResultAsync(f.health, f.firstFailure, false)
	}
}
