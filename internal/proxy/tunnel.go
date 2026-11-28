package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/outbound"
	"github.com/Resinat/Resin/internal/routing"
	M "github.com/sagernet/sing/common/metadata"
)

type tunnelDeps struct {
	router      *routing.Router
	pool        outbound.PoolAccessor
	health      HealthRecorder
	metricsSink MetricsEventSink
	bypass      *TargetBypassMatcher
}

type preparedTunnel struct {
	upstreamConn net.Conn
	onFirstByte  func()
	recordResult func(tunnelRelayResult)
}

func (t *preparedTunnel) finish(result tunnelRelayResult) {
	if t.recordResult != nil {
		t.recordResult(result)
	}
}

type tunnelPrepareResult struct {
	session       *preparedTunnel
	proxyErr      *ProxyError
	upstreamStage string
	upstreamErr   error
	canceled      bool
}

type tunnelRelayResult struct {
	ingressBytes         int64
	egressBytes          int64
	netOK                bool
	proxyErr             *ProxyError
	upstreamStage        string
	upstreamErr          error
	upstreamResponded    bool
	earlyUpstreamFailure bool
}

type tunnelPumpOptions struct {
	requireBidirectionalTraffic bool
	onFirstIngressByte          func()
}

type firstByteReader struct {
	reader      io.Reader
	onFirstByte func()
	once        sync.Once
	onEnd       func(error)
}

func (r *firstByteReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 && r.onFirstByte != nil {
		r.once.Do(r.onFirstByte)
	}
	if err != nil && r.onEnd != nil {
		r.onEnd(err)
	}
	return n, err
}

type tunnelWriteObserver struct {
	io.Writer
	onError func()
}

func (w tunnelWriteObserver) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err != nil {
		w.onError()
	}
	return n, err
}

func prepareConnectTunnel(
	ctx context.Context,
	deps tunnelDeps,
	platformName string,
	account string,
	target string,
	lifecycle *requestLifecycle,
) tunnelPrepareResult {
	if deps.bypass != nil && deps.bypass.ShouldBypass(target) {
		return prepareDirectConnectTunnel(ctx, deps, target)
	}

	attempts, routeErr := selectOutboundAttempts(deps.router, deps.pool, platformName, account, target)
	if routeErr != nil {
		return tunnelPrepareResult{proxyErr: routeErr}
	}

	domain := netutil.ExtractDomain(target)
	destination := M.ParseSocksaddr(target)
	var dialErr error
	feedback := &attemptFeedback{attempts: attempts, health: deps.health, lifecycle: lifecycle}
	for routed := range attempts.routes(ctx) {
		route := routed.Route
		start := time.Now()
		if lifecycle != nil {
			lifecycle.startRouteAttempt(route)
		}
		rawConn, err := dialOutbound(ctx, outboundDialTimeout, routed.Outbound, routed.stalledDials, "tcp", destination)
		if err == nil {
			if ctx.Err() != nil {
				_ = rawConn.Close()
				break
			}
			return prepareRoutedTunnel(ctx, deps, feedback, route, rawConn, domain, start)
		}
		dialErr = err
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			break
		}
		feedback.failed(route, "connect_dial", err, true)
	}
	if ctx.Err() != nil {
		dialErr = ctx.Err()
	} else if !errors.Is(dialErr, context.Canceled) {
		feedback.finish(false)
	}
	if dialErr == nil {
		return tunnelPrepareResult{proxyErr: ErrNoAvailableNodes}
	}
	proxyErr := classifyConnectError(dialErr)
	return tunnelPrepareResult{
		proxyErr:      proxyErr,
		upstreamStage: "connect_dial",
		upstreamErr:   dialErr,
		canceled:      proxyErr == nil,
	}
}

func prepareRoutedTunnel(ctx context.Context, deps tunnelDeps, feedback *attemptFeedback, route routing.RouteResult, rawConn net.Conn, domain string, start time.Time) tunnelPrepareResult {
	var upstreamBase net.Conn = rawConn
	if deps.metricsSink != nil {
		deps.metricsSink.OnConnectionLifecycle(ConnectionOutbound, ConnectionOpen)
		upstreamBase = newCountingConn(rawConn, deps.metricsSink)
	}

	upstreamConn := newTLSLatencyConn(upstreamBase, func(latency time.Duration) {
		if deps.health != nil {
			deps.health.RecordLatency(route.NodeHash, domain, &latency)
		}
	})

	return tunnelPrepareResult{
		session: &preparedTunnel{
			upstreamConn: upstreamConn,
			onFirstByte: func() {
				if feedback.lifecycle != nil {
					feedback.lifecycle.markUpstreamFirstByte(start)
				}
				if ctx.Err() == nil {
					feedback.confirm(route)
				}
			},
			recordResult: func(result tunnelRelayResult) {
				if ctx.Err() != nil {
					return
				}
				if !result.netOK || result.earlyUpstreamFailure {
					feedback.failed(route, result.upstreamStage, result.upstreamErr, result.earlyUpstreamFailure)
				}
				feedback.finish(result.upstreamResponded)
				if result.netOK && result.upstreamResponded {
					recordPassiveResultAsync(deps.health, route, true)
				}
			},
		},
	}
}

func prepareDirectConnectTunnel(ctx context.Context, deps tunnelDeps, target string) tunnelPrepareResult {
	var dialer net.Dialer
	rawConn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		proxyErr := classifyConnectError(err)
		if proxyErr == nil {
			return tunnelPrepareResult{canceled: true}
		}
		return tunnelPrepareResult{
			proxyErr:      proxyErr,
			upstreamStage: "connect_direct_dial",
			upstreamErr:   err,
		}
	}

	var upstreamConn net.Conn = rawConn
	if deps.metricsSink != nil {
		deps.metricsSink.OnConnectionLifecycle(ConnectionOutbound, ConnectionOpen)
		upstreamConn = newCountingConn(rawConn, deps.metricsSink)
	}
	return tunnelPrepareResult{
		session: &preparedTunnel{
			upstreamConn: upstreamConn,
		},
	}
}

func pumpPreparedTunnel(
	clientConn net.Conn,
	clientReader *bufio.Reader,
	session *preparedTunnel,
	opts tunnelPumpOptions,
) tunnelRelayResult {
	clientToUpstream, err := makeTunnelClientReader(clientConn, clientReader)
	if err != nil {
		if session != nil && session.upstreamConn != nil {
			_ = session.upstreamConn.Close()
		}
		if clientConn != nil {
			_ = clientConn.Close()
		}
		return tunnelRelayResult{
			proxyErr:      ErrUpstreamRequestFailed,
			upstreamStage: "connect_client_prefetch_drain",
			upstreamErr:   err,
		}
	}
	return pumpPreparedTunnelReader(clientConn, clientToUpstream, session, opts)
}

func pumpPreparedTunnelReader(
	clientConn net.Conn,
	clientToUpstream io.Reader,
	session *preparedTunnel,
	opts tunnelPumpOptions,
) tunnelRelayResult {
	if clientConn == nil || clientToUpstream == nil || session == nil || session.upstreamConn == nil {
		return tunnelRelayResult{}
	}

	type copyResult struct {
		n   int64
		err error
	}
	var closeBothOnce sync.Once
	closeBoth := func() {
		closeBothOnce.Do(func() {
			_ = clientConn.Close()
			_ = session.upstreamConn.Close()
		})
	}
	ingressBytesCh := make(chan copyResult, 1)
	egressBytesCh := make(chan copyResult, 1)
	var clientReadEnded atomic.Bool
	var upstreamEndedEarly, upstreamWriteFailed, upstreamResponded bool
	go func() {
		clientReader := &firstByteReader{reader: clientToUpstream, onEnd: func(error) { clientReadEnded.Store(true) }}
		upstreamWriter := tunnelWriteObserver{Writer: session.upstreamConn, onError: func() { upstreamWriteFailed = true }}
		n, copyErr := io.Copy(upstreamWriter, clientReader)
		if !isBenignTunnelCopyError(copyErr) || !closeWriteConn(session.upstreamConn) {
			closeBoth()
		}
		egressBytesCh <- copyResult{n: n, err: copyErr}
	}()
	go func() {
		upstreamReader := &firstByteReader{
			reader: session.upstreamConn,
			onFirstByte: func() {
				upstreamResponded = true
				if opts.onFirstIngressByte != nil {
					opts.onFirstIngressByte()
				}
				if session.onFirstByte != nil {
					session.onFirstByte()
				}
			},
			onEnd: func(err error) {
				upstreamEndedEarly = !clientReadEnded.Load() && !errors.Is(err, context.Canceled)
			},
		}
		n, copyErr := io.Copy(clientConn, upstreamReader)
		if !isBenignTunnelCopyError(copyErr) || !closeWriteConn(clientConn) {
			closeBoth()
		}
		ingressBytesCh <- copyResult{n: n, err: copyErr}
	}()

	ingressResult := <-ingressBytesCh
	egressResult := <-egressBytesCh
	closeBoth()

	ingressErrBenign := isBenignTunnelCopyError(ingressResult.err)
	egressErrBenign := isBenignTunnelCopyError(egressResult.err)
	// A client-side TCP reset after the upstream response has already started is
	// a shutdown artifact, not an upstream failure. This commonly happens when a
	// tunnel client exits immediately after consuming the response.
	if !egressErrBenign && ingressResult.n > 0 && isClientReadResetError(egressResult.err) {
		egressErrBenign = true
	}

	result := tunnelRelayResult{
		ingressBytes:      ingressResult.n,
		egressBytes:       egressResult.n,
		netOK:             true,
		upstreamResponded: upstreamResponded,
		earlyUpstreamFailure: !upstreamResponded && (upstreamWriteFailed ||
			(egressResult.n > 0 && (upstreamEndedEarly || opts.requireBidirectionalTraffic))),
	}
	switch {
	case !ingressErrBenign:
		result.netOK = false
		result.proxyErr = ErrUpstreamRequestFailed
		result.upstreamStage = "connect_upstream_to_client_copy"
		result.upstreamErr = ingressResult.err
	case !egressErrBenign:
		result.netOK = false
		result.proxyErr = ErrUpstreamRequestFailed
		result.upstreamStage = "connect_client_to_upstream_copy"
		result.upstreamErr = egressResult.err
	case result.earlyUpstreamFailure || (opts.requireBidirectionalTraffic && (ingressResult.n == 0 || egressResult.n == 0)):
		result.netOK = false
		result.proxyErr = ErrUpstreamRequestFailed
		switch {
		case ingressResult.n == 0 && egressResult.n == 0:
			result.upstreamStage = "connect_zero_traffic"
		case ingressResult.n == 0:
			result.upstreamStage = "connect_no_ingress_traffic"
		default:
			result.upstreamStage = "connect_no_egress_traffic"
		}
	}
	return result
}

func closeWriteConn(conn net.Conn) bool {
	return closeWriteErr(conn) == nil
}

// makeTunnelClientReader returns a reader for client->upstream copy that
// preserves any bytes already buffered by a protocol reader before tunneling.
func makeTunnelClientReader(clientConn net.Conn, buffered *bufio.Reader) (io.Reader, error) {
	if buffered == nil {
		return clientConn, nil
	}
	n := buffered.Buffered()
	if n == 0 {
		return clientConn, nil
	}
	prefetched := make([]byte, n)
	if _, err := io.ReadFull(buffered, prefetched); err != nil {
		return nil, err
	}
	return io.MultiReader(bytes.NewReader(prefetched), clientConn), nil
}
