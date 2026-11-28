package routing

import (
	"context"
	"iter"
	"math/rand/v2"
	"time"

	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/puzpuzpuz/xsync/v4"
)

// AlternateRoutes prefers a lease another request has already established,
// then the original exit, then bounded samples from the platform.
func (r *Router) AlternateRoutes(ctx context.Context, account string, first RouteResult, target string) iter.Seq[RouteResult] {
	return func(yield func(RouteResult) bool) {
		plat, ok := r.pool.GetPlatform(first.PlatformID)
		if !ok {
			return
		}
		state, ok := r.states.Load(plat.ID)
		if !ok {
			return
		}
		domain := netutil.ExtractDomain(target)
		tried := map[node.Hash]struct{}{first.NodeHash: {}}
		for ctx.Err() == nil {
			route, found := r.currentLeaseRoute(plat, state, account, tried)
			if !found {
				hash, sameIP := chooseSameIPRotationCandidate(plat, r.pool, first.EgressIP, domain, r.authorities(), r.p2cWindow(), tried)
				if sameIP {
					tried[hash] = struct{}{}
					route, found = r.alternateRoute(plat, hash)
					found = found && route.EgressIP == first.EgressIP
				}
			}
			if !found {
				route, found = r.pickAlternate(plat, state, domain, tried)
			}
			if !found || ctx.Err() != nil {
				return
			}
			if current, ok := r.pool.GetPlatform(plat.ID); !ok || current != plat {
				return
			}
			tried[route.NodeHash] = struct{}{}
			if r.nodeTagResolver != nil {
				route.NodeTag = r.nodeTagResolver(route.NodeHash)
			}
			if !yield(route) {
				return
			}
		}
	}
}

func (r *Router) currentLeaseRoute(plat *platform.Platform, state *PlatformRoutingState, account string, tried map[node.Hash]struct{}) (RouteResult, bool) {
	if account == "" {
		return RouteResult{}, false
	}
	current, ok := state.Leases.GetLease(account)
	if !ok || current.needsRotation || current.IsExpired(time.Now()) {
		return RouteResult{}, false
	}
	if _, attempted := tried[current.NodeHash]; attempted {
		return RouteResult{}, false
	}
	route, ok := r.alternateRoute(plat, current.NodeHash)
	if !ok || route.EgressIP != current.EgressIP {
		return RouteResult{}, false
	}
	route.leaseCreatedAtNs = current.CreatedAtNs
	route.leaseExpiryNs = current.ExpiryNs
	route.bindingRevision = current.bindingRevision
	return route, true
}

const (
	alternateSampleLimit = 16
	smallRoutableView    = 32
)

func (r *Router) pickAlternate(plat *platform.Platform, state *PlatformRoutingState, domain string, tried map[node.Hash]struct{}) (RouteResult, bool) {
	view := plat.View()
	if view.Size() == 0 {
		return RouteResult{}, false
	}
	var candidates [2]RouteResult
	n := 0
	consider := func(hash node.Hash) {
		if _, attempted := tried[hash]; attempted || (n > 0 && hash == candidates[0].NodeHash) {
			return
		}
		if route, ok := r.alternateRoute(plat, hash); ok {
			candidates[n] = route
			n++
		}
	}
	rng := randomRouteRNGPool.Get().(*rand.Rand)
	defer randomRouteRNGPool.Put(rng)
	for i := 0; i < alternateSampleLimit && n < len(candidates); i++ {
		if hash, ok := view.RandomPick(rng); ok {
			consider(hash)
		}
	}
	// Ensure a lone eligible node in a small pool is not missed by sampling.
	if n < len(candidates) && view.Size() <= smallRoutableView {
		var hashes [smallRoutableView]node.Hash
		size := 0
		view.Range(func(hash node.Hash) bool {
			hashes[size] = hash
			size++
			return size < len(hashes)
		})
		for _, hash := range hashes[:size] {
			consider(hash)
			if n == len(candidates) {
				break
			}
		}
	}
	if n == 2 && preferRoute(candidates[1].NodeHash, candidates[0].NodeHash, plat, state.IPLoadStats, r.pool, domain, r.authorities(), r.p2cWindow()) {
		return candidates[1], true
	}
	return candidates[0], n > 0
}

func (r *Router) alternateRoute(plat *platform.Platform, hash node.Hash) (RouteResult, bool) {
	if !plat.View().Contains(hash) {
		return RouteResult{}, false
	}
	entry, ok := r.pool.GetEntry(hash)
	if !ok || !entry.IsHealthy() {
		return RouteResult{}, false
	}
	ip := entry.GetEgressIP()
	if !ip.IsValid() {
		return RouteResult{}, false
	}
	return withPlatformContext(plat, RouteResult{NodeHash: hash, EgressIP: ip}), true
}

func (route RouteResult) matchesLease(current Lease, now time.Time) bool {
	return !current.IsExpired(now) && current.NodeHash == route.NodeHash &&
		current.EgressIP == route.EgressIP && current.CreatedAtNs == route.leaseCreatedAtNs &&
		current.ExpiryNs == route.leaseExpiryNs && current.bindingRevision == route.bindingRevision
}

// HasLeaseBinding reports whether this route observed an active sticky lease.
func (route RouteResult) HasLeaseBinding() bool {
	return route.leaseExpiryNs != 0
}

// NoteLeaseFailure makes the next request reconsider this binding if it is
// still active. The failure is local to the account and never opens a circuit.
func (r *Router) NoteLeaseFailure(account string, route RouteResult) {
	if account == "" || !route.HasLeaseBinding() {
		return
	}
	state, ok := r.states.Load(route.PlatformID)
	if !ok {
		return
	}
	state.Leases.leases.Compute(account, func(current Lease, loaded bool) (Lease, xsync.ComputeOp) {
		if !loaded || !route.matchesLease(current, time.Now()) || current.needsRotation {
			return current, xsync.CancelOp
		}
		current.needsRotation = true
		return current, xsync.UpdateOp
	})
}

// CommitFailover confirms the connected route only while the observed binding
// is still current, preserving its TTL for same-exit changes.
func (r *Router) CommitFailover(account string, first, connected RouteResult) {
	if account == "" || !first.HasLeaseBinding() || first.PlatformID != connected.PlatformID {
		return
	}
	plat, ok := r.pool.GetPlatform(first.PlatformID)
	if !ok {
		return
	}
	state, ok := r.states.Load(plat.ID)
	if !ok {
		return
	}
	state.Leases.leases.Compute(account, func(current Lease, loaded bool) (Lease, xsync.ComputeOp) {
		now := time.Now()
		if !loaded || !first.matchesLease(current, now) {
			return current, xsync.CancelOp
		}
		if first.NodeHash == connected.NodeHash {
			if !current.needsRotation {
				return current, xsync.CancelOp
			}
			current.needsRotation = false
			return current, xsync.UpdateOp
		}
		entry, ok := r.pool.GetEntry(connected.NodeHash)
		if !ok || !entry.IsHealthy() || !plat.View().Contains(connected.NodeHash) || entry.GetEgressIP() != connected.EgressIP {
			return current, xsync.CancelOp
		}
		return r.replaceLeaseRoute(plat, state, account, current, connected, now), xsync.UpdateOp
	})
}
