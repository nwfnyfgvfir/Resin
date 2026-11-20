package proxy

import (
	"context"
	"iter"

	"github.com/Resinat/Resin/internal/outbound"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/sagernet/sing-box/adapter"
)

type routedOutbound struct {
	Route    routing.RouteResult
	Outbound adapter.Outbound
}

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
		if first, ok := a.outbound(a.first); ok && !yield(first) {
			return
		}
		for route := range a.router.AlternateRoutes(ctx, a.first, a.target) {
			if next, ok := a.outbound(route); ok && !yield(next) {
				return
			}
		}
	}
}

func (a outboundAttempts) outbound(result routing.RouteResult) (routedOutbound, bool) {
	entry, ok := a.pool.GetEntry(result.NodeHash)
	if !ok {
		return routedOutbound{}, false
	}
	obPtr := entry.Outbound.Load()
	if obPtr == nil {
		return routedOutbound{}, false
	}

	return routedOutbound{
		Route:    result,
		Outbound: *obPtr,
	}, true
}
