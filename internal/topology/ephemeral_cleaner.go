package topology

import (
	"log"
	"runtime"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/scanloop"
	"github.com/Resinat/Resin/internal/subscription"
)

// EphemeralCleaner periodically removes unhealthy nodes from subscriptions.
// Ephemeral subscriptions always participate; non-ephemeral subscriptions
// participate when global auto-remove is enabled in runtime config.
type EphemeralCleaner struct {
	subManager    *SubscriptionManager
	pool          *GlobalNodePool
	onNodeEvicted func(subID string, hash node.Hash)

	autoRemoveUnhealthyNodesEnabled func() bool
	autoRemoveUnhealthyNodesDelay   func() time.Duration
	autoDeleteEmptySubscriptions    func() bool

	onEmptySubscription func(subID string)

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewEphemeralCleaner creates an EphemeralCleaner that reads per-subscription
// eviction delay values during each sweep.
func NewEphemeralCleaner(
	subManager *SubscriptionManager,
	pool *GlobalNodePool,
) *EphemeralCleaner {
	return &EphemeralCleaner{
		subManager: subManager,
		pool:       pool,
		stopCh:     make(chan struct{}),
	}
}

// SetOnNodeEvicted sets callback invoked for each newly-evicted node.
func (c *EphemeralCleaner) SetOnNodeEvicted(fn func(subID string, hash node.Hash)) {
	c.onNodeEvicted = fn
}

// SetGlobalAutoRemove configures global auto-removal for non-ephemeral subscriptions.
func (c *EphemeralCleaner) SetGlobalAutoRemove(
	enabled func() bool,
	delay func() time.Duration,
) {
	c.autoRemoveUnhealthyNodesEnabled = enabled
	c.autoRemoveUnhealthyNodesDelay = delay
}

// SetAutoDeleteEmptySubscriptions configures whether subscriptions with no active
// nodes should be deleted after auto-removal.
func (c *EphemeralCleaner) SetAutoDeleteEmptySubscriptions(enabled func() bool) {
	c.autoDeleteEmptySubscriptions = enabled
}

// SetOnEmptySubscription sets callback invoked when an eligible subscription
// has no active nodes and should be deleted.
func (c *EphemeralCleaner) SetOnEmptySubscription(fn func(subID string)) {
	c.onEmptySubscription = fn
}

// Start launches the background cleaner goroutine.
func (c *EphemeralCleaner) Start() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		scanloop.Run(c.stopCh, scanloop.DefaultMinInterval, scanloop.DefaultJitterRange, c.sweep)
	}()
}

// Stop signals the cleaner to stop and waits for it to finish.
func (c *EphemeralCleaner) Stop() {
	close(c.stopCh)
	c.wg.Wait()
}

func (c *EphemeralCleaner) sweep() {
	c.sweepWithHook(nil)
}

// sweepWithHook runs the sweep. If betweenScans is non-nil, it is called
// after the candidate set (evictSet) is built but before the second
// verification check. This allows tests to inject state changes at the
// exact TOCTOU window.
func (c *EphemeralCleaner) sweepWithHook(betweenScans func()) {
	now := time.Now().UnixNano()

	type subscriptionTarget struct {
		id  string
		sub *subscription.Subscription
	}
	targetSubs := make([]subscriptionTarget, 0, c.subManager.Size())
	globalAutoRemove := c.globalAutoRemoveEnabled()

	c.subManager.Range(func(id string, sub *subscription.Subscription) bool {
		if sub.Ephemeral() || globalAutoRemove {
			targetSubs = append(targetSubs, subscriptionTarget{id: id, sub: sub})
		}
		return true
	})

	if len(targetSubs) == 0 {
		return
	}

	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	if workers > len(targetSubs) {
		workers = len(targetSubs)
	}

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, item := range targetSubs {
		sem <- struct{}{}
		wg.Add(1)
		go func(id string, sub *subscription.Subscription) {
			defer wg.Done()
			defer func() { <-sem }()
			c.sweepOneSubscription(id, sub, now, betweenScans)
		}(item.id, item.sub)
	}
	wg.Wait()
}

func (c *EphemeralCleaner) sweepOneSubscription(
	id string,
	sub *subscription.Subscription,
	now int64,
	betweenScans func(),
) {
	var (
		evictCount    int
		evictedHashes []node.Hash
	)
	sub.WithOpLock(func() {
		evictDelayNs := c.evictDelayNs(sub)
		evictCount, evictedHashes = CleanupSubscriptionNodesWithConfirmNoLock(
			sub,
			c.pool,
			func(entry *node.NodeEntry) bool {
				return c.shouldEvictEntry(entry, now, evictDelayNs)
			},
			betweenScans,
		)
	})
	if c.onNodeEvicted != nil {
		for _, h := range evictedHashes {
			c.onNodeEvicted(id, h)
		}
	}

	if evictCount > 0 {
		log.Printf("[unhealthy-node-cleaner] evicted %d nodes from sub %s", evictCount, id)
	}

	c.maybeDeleteEmptySubscription(id, sub)
}

func (c *EphemeralCleaner) maybeDeleteEmptySubscription(id string, sub *subscription.Subscription) {
	if c.onEmptySubscription == nil || !c.autoDeleteEmptySubscriptionsEnabled() {
		return
	}
	if sub == nil || !c.participatesInAutoRemove(sub) {
		return
	}
	if !SubscriptionHasNoActiveNodes(sub) {
		return
	}
	c.onEmptySubscription(id)
}

func (c *EphemeralCleaner) autoDeleteEmptySubscriptionsEnabled() bool {
	if c.autoDeleteEmptySubscriptions == nil {
		return false
	}
	return c.autoDeleteEmptySubscriptions()
}

func (c *EphemeralCleaner) participatesInAutoRemove(sub *subscription.Subscription) bool {
	return sub.Ephemeral() || c.globalAutoRemoveEnabled()
}

func (c *EphemeralCleaner) globalAutoRemoveEnabled() bool {
	if c.autoRemoveUnhealthyNodesEnabled == nil {
		return false
	}
	return c.autoRemoveUnhealthyNodesEnabled()
}

func (c *EphemeralCleaner) evictDelayNs(sub *subscription.Subscription) int64 {
	if sub.Ephemeral() {
		return sub.EphemeralNodeEvictDelayNs()
	}
	if c.autoRemoveUnhealthyNodesDelay != nil {
		return int64(c.autoRemoveUnhealthyNodesDelay())
	}
	return 0
}

func (c *EphemeralCleaner) shouldEvictEntry(entry *node.NodeEntry, now int64, evictDelayNs int64) bool {
	if entry == nil {
		return false
	}

	// Outbound build failed and node is still without outbound.
	// For ephemeral subscriptions, this node should be dropped quickly.
	if !entry.HasOutbound() && entry.GetLastError() != "" {
		return true
	}

	// Circuit remains open beyond configured eviction delay.
	circuitSince := entry.CircuitOpenSince.Load()
	return circuitSince > 0 && (now-circuitSince) > evictDelayNs
}
