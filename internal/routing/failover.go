package routing

import (
	"cmp"
	"context"
	"iter"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/node"
	"github.com/puzpuzpuz/xsync/v4"
)

// AlternateRoutes takes one pass over the original platform's routable view.
// It ranks nodes on the original exit by latency, then uses the platform's
// allocation policy to choose among the remaining exits without replacement.
func (r *Router) AlternateRoutes(ctx context.Context, first RouteResult, target string) iter.Seq[RouteResult] {
	return func(yield func(RouteResult) bool) {
		plat, ok := r.pool.GetPlatform(first.PlatformID)
		if !ok || ctx.Err() != nil {
			return
		}
		state, ok := r.states.Load(plat.ID)
		if !ok {
			return
		}
		view := plat.View()
		domain := netutil.ExtractDomain(target)
		authorities, window := r.authorities(), r.p2cWindow()
		type candidate struct {
			hash       node.Hash
			latency    time.Duration
			hasLatency bool
		}
		var sameIP []candidate
		otherIPs := make([]node.Hash, 0, view.Size())
		view.Range(func(hash node.Hash) bool {
			if ctx.Err() != nil {
				return false
			}
			if hash == first.NodeHash {
				return true
			}
			entry, ok := r.pool.GetEntry(hash)
			if !ok || !entry.IsHealthy() {
				return true
			}
			if entry.GetEgressIP() == first.EgressIP {
				latency, hasLatency := sameIPCandidateLatency(entry, domain, authorities, window)
				sameIP = append(sameIP, candidate{hash, latency, hasLatency})
			} else {
				otherIPs = append(otherIPs, hash)
			}
			return true
		})

		if ctx.Err() != nil {
			return
		}
		slices.SortStableFunc(sameIP, func(a, b candidate) int {
			if a.hasLatency == b.hasLatency {
				return cmp.Compare(a.latency, b.latency)
			}
			if a.hasLatency {
				return -1
			}
			return 1
		})
		for _, candidate := range sameIP {
			if ctx.Err() != nil {
				return
			}
			if route, ok := r.alternateRoute(first.PlatformID, candidate.hash); ok {
				if route.EgressIP != first.EgressIP {
					otherIPs = append(otherIPs, candidate.hash)
				} else if !yield(route) {
					return
				}
			}
		}

		for len(otherIPs) > 0 && ctx.Err() == nil {
			n := len(otherIPs)
			pick := rand.IntN(n)
			if n > 1 {
				peer := (pick + 1 + rand.IntN(n-1)) % n
				if preferRoute(otherIPs[peer], otherIPs[pick], plat, state.IPLoadStats, r.pool, domain, authorities, window) {
					pick = peer
				}
			}
			hash := otherIPs[pick]
			otherIPs[pick] = otherIPs[n-1]
			otherIPs = otherIPs[:n-1]
			if route, ok := r.alternateRoute(first.PlatformID, hash); ok && !yield(route) {
				return
			}
		}
	}
}

func (r *Router) alternateRoute(platformID string, hash node.Hash) (RouteResult, bool) {
	plat, ok := r.pool.GetPlatform(platformID)
	if !ok || !plat.View().Contains(hash) {
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
	result := withPlatformContext(plat, RouteResult{NodeHash: hash, EgressIP: ip})
	if r.nodeTagResolver != nil {
		result.NodeTag = r.nodeTagResolver(hash)
	}
	return result, true
}

// CommitFailover updates a sticky lease after an alternate succeeds, provided
// the original binding has not expired or been replaced in the meantime.
func (r *Router) CommitFailover(account string, first, connected RouteResult) {
	if account == "" || first.PlatformID != connected.PlatformID || first.NodeHash == connected.NodeHash {
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
		if !loaded || current.IsExpired(now) || current.NodeHash != first.NodeHash ||
			current.EgressIP != first.EgressIP || current.CreatedAtNs != first.leaseCreatedAtNs ||
			current.ExpiryNs != first.leaseExpiryNs {
			return current, xsync.CancelOp
		}
		entry, ok := r.pool.GetEntry(connected.NodeHash)
		if !ok || !entry.IsHealthy() || !plat.View().Contains(connected.NodeHash) || entry.GetEgressIP() != connected.EgressIP {
			return current, xsync.CancelOp
		}
		if current.EgressIP == connected.EgressIP {
			return r.replaceLeaseNode(plat.ID, account, current, connected.NodeHash, now.UnixNano()), xsync.UpdateOp
		}
		r.cleanupPreviousLease(state, current, true, leaseInvalidationRemove, plat.ID, account)
		replacement := newStickyLease(plat, connected.NodeHash, connected.EgressIP, now)
		r.recordLeaseCreate(state, plat.ID, account, replacement)
		return replacement, xsync.UpdateOp
	})
}
