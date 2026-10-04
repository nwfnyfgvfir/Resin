package topology

import (
	"errors"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/state"
	"github.com/Resinat/Resin/internal/subscription"
)

// SubscriptionActiveNodeCount returns managed nodes that are not evicted.
func SubscriptionActiveNodeCount(sub *subscription.Subscription) int {
	if sub == nil {
		return 0
	}
	managed := sub.ManagedNodes()
	if managed == nil {
		return 0
	}
	count := 0
	managed.RangeNodes(func(_ node.Hash, n subscription.ManagedNode) bool {
		if !n.Evicted {
			count++
		}
		return true
	})
	return count
}

// SubscriptionHasNoActiveNodes reports whether a subscription has no routable
// managed nodes but has previously loaded content or managed entries.
func SubscriptionHasNoActiveNodes(sub *subscription.Subscription) bool {
	if sub == nil {
		return false
	}
	if SubscriptionActiveNodeCount(sub) > 0 {
		return false
	}
	if sub.LastUpdatedNs.Load() > 0 {
		return true
	}
	managed := sub.ManagedNodes()
	return managed != nil && managed.Size() > 0
}

// DeleteSubscriptionRuntime removes a subscription from persistence and runtime.
func DeleteSubscriptionRuntime(
	engine *state.StateEngine,
	subMgr *SubscriptionManager,
	pool *GlobalNodePool,
	id string,
) error {
	sub := subMgr.Lookup(id)
	if sub == nil {
		return state.ErrNotFound
	}

	var (
		managedHashes []node.Hash
		deleteErr     error
	)

	sub.WithOpLock(func() {
		lockedSub := subMgr.Lookup(id)
		if lockedSub == nil {
			deleteErr = state.ErrNotFound
			return
		}

		lockedSub.ManagedNodes().RangeNodes(func(h node.Hash, _ subscription.ManagedNode) bool {
			managedHashes = append(managedHashes, h)
			return true
		})

		if engine != nil {
			if err := engine.DeleteSubscription(id); err != nil {
				deleteErr = err
				return
			}
		}

		for _, h := range managedHashes {
			pool.RemoveNodeFromSub(h, id)
		}
		subMgr.Unregister(id)
	})

	return deleteErr
}

// IsSubscriptionNotFound reports whether err indicates the subscription is gone.
func IsSubscriptionNotFound(err error) bool {
	return errors.Is(err, state.ErrNotFound)
}
