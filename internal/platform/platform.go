package platform

import (
	"net/netip"
	"regexp"
	"slices"
	"sync"

	"github.com/Resinat/Resin/internal/node"
)

// DefaultPlatformID is the well-known UUID of the built-in Default platform.
const DefaultPlatformID = "00000000-0000-0000-0000-000000000000"

// DefaultPlatformName is the built-in platform name.
const DefaultPlatformName = "Default"

// GeoLookupFunc resolves an IP address to a lowercase ISO country code.
type GeoLookupFunc func(netip.Addr) string

// PoolRangeFunc iterates all nodes in the global pool.
type PoolRangeFunc func(fn func(node.Hash, *node.NodeEntry) bool)

// GetEntryFunc retrieves a node entry from the global pool by hash.
type GetEntryFunc func(node.Hash) (*node.NodeEntry, bool)

// Platform represents a routing platform with its filtered routable view.
type Platform struct {
	ID   string
	Name string

	// Filter configuration.
	RegexFilters  node.TagFilter
	RegionFilters []string // lowercase ISO codes, supports negation "!xx"

	// Other config fields.
	StickyTTLNs                      int64
	ReverseProxyMissAction           string
	ReverseProxyEmptyAccountBehavior string
	ReverseProxyFixedAccountHeader   string
	ReverseProxyFixedAccountHeaders  []string
	AllocationPolicy                 AllocationPolicy
	PassiveCircuitBreakerDisabled    bool

	// Routable view & its lock.
	// viewMu serializes both FullRebuild and NotifyDirty.
	view            *RoutableView
	viewMu          sync.RWMutex
	nodesByEgressIP map[netip.Addr][]node.Hash
	egressPositions map[node.Hash]egressPosition
}

type egressPosition struct {
	ip    netip.Addr
	index int
}

// NewPlatform creates a Platform with an empty routable view.
// The regex slice is treated as MUST rules for compatibility with internal callers.
func NewPlatform(id, name string, regexFilters []*regexp.Regexp, regionFilters []string) *Platform {
	return NewPlatformWithTagFilter(id, name, node.TagFilter{Must: regexFilters}, regionFilters)
}

// NewPlatformWithTagFilter creates a Platform with compiled line-oriented tag rules.
func NewPlatformWithTagFilter(id, name string, regexFilters node.TagFilter, regionFilters []string) *Platform {
	return &Platform{
		ID:              id,
		Name:            name,
		RegexFilters:    regexFilters,
		RegionFilters:   regionFilters,
		view:            NewRoutableView(),
		nodesByEgressIP: make(map[netip.Addr][]node.Hash),
		egressPositions: make(map[node.Hash]egressPosition),
	}
}

// View returns the platform's routable view as a read-only interface.
// External callers cannot Add/Remove/Clear — only FullRebuild and NotifyDirty can mutate.
func (p *Platform) View() ReadOnlyView {
	return p.view
}

// EgressNodes returns a snapshot of the routable nodes indexed for ip.
func (p *Platform) EgressNodes(ip netip.Addr) []node.Hash {
	if !ip.IsValid() {
		return nil
	}
	p.viewMu.RLock()
	defer p.viewMu.RUnlock()
	return slices.Clone(p.nodesByEgressIP[ip])
}

// FullRebuild clears the routable view and re-evaluates all nodes from the pool.
// Acquires viewMu — any concurrent NotifyDirty calls block until rebuild completes.
func (p *Platform) FullRebuild(
	poolRange PoolRangeFunc,
	subLookup node.SubLookupFunc,
	geoLookup GeoLookupFunc,
) {
	p.viewMu.Lock()
	defer p.viewMu.Unlock()

	p.view.Clear()
	clear(p.nodesByEgressIP)
	clear(p.egressPositions)
	poolRange(func(h node.Hash, entry *node.NodeEntry) bool {
		p.setRoutableIP(h, p.evaluateNode(entry, subLookup, geoLookup))
		return true
	})
}

// NotifyDirty re-evaluates a single node and adds/removes it from the view.
// Acquires viewMu — serialized with FullRebuild.
func (p *Platform) NotifyDirty(
	h node.Hash,
	getEntry GetEntryFunc,
	subLookup node.SubLookupFunc,
	geoLookup GeoLookupFunc,
) {
	p.viewMu.Lock()
	defer p.viewMu.Unlock()

	entry, ok := getEntry(h)
	var ip netip.Addr
	if ok {
		ip = p.evaluateNode(entry, subLookup, geoLookup)
	}
	p.setRoutableIP(h, ip)
}

// setRoutableIP updates both indexes under viewMu.
func (p *Platform) setRoutableIP(h node.Hash, ip netip.Addr) {
	if pos, ok := p.egressPositions[h]; ok {
		if pos.ip == ip {
			return
		}
		bucket := p.nodesByEgressIP[pos.ip]
		last := len(bucket) - 1
		if pos.index != last {
			moved := bucket[last]
			bucket[pos.index] = moved
			p.egressPositions[moved] = pos
		}
		if last == 0 {
			delete(p.nodesByEgressIP, pos.ip)
		} else {
			p.nodesByEgressIP[pos.ip] = bucket[:last]
		}
		delete(p.egressPositions, h)
	}
	if !ip.IsValid() {
		p.view.Remove(h)
		return
	}
	bucket := p.nodesByEgressIP[ip]
	p.egressPositions[h] = egressPosition{ip: ip, index: len(bucket)}
	p.nodesByEgressIP[ip] = append(bucket, h)
	p.view.Add(h)
}

// evaluateNode returns the indexed egress IP if the node is routable.
func (p *Platform) evaluateNode(
	entry *node.NodeEntry,
	subLookup node.SubLookupFunc,
	geoLookup GeoLookupFunc,
) netip.Addr {
	// 0. Disabled nodes are never routable.
	if entry.IsDisabledBySubscriptions(subLookup) {
		return netip.Addr{}
	}

	// 1. Healthy for routing (outbound ready + circuit not open).
	if !entry.IsHealthy() {
		return netip.Addr{}
	}

	// 2. Tag regex match.
	if !entry.MatchTagFilter(p.RegexFilters, subLookup) {
		return netip.Addr{}
	}

	// 3. Egress IP must be known.
	egressIP := entry.GetEgressIP()
	if !egressIP.IsValid() {
		return netip.Addr{}
	}

	// 4. Region filter (when configured).
	if len(p.RegionFilters) > 0 {
		region := entry.GetRegion(geoLookup)
		if !MatchRegionFilter(region, p.RegionFilters) {
			return netip.Addr{}
		}
	}

	// 5. Has at least one latency record.
	if !entry.HasLatency() {
		return netip.Addr{}
	}

	return egressIP
}

// MatchRegionFilter applies include/exclude region filters.
// Positive entries (xx) build an include set; negative entries (!xx) build an exclude set.
// Unknown regions never match when region filters are configured.
// Final result is: region known AND (include empty OR region in include) AND (region not in exclude).
func MatchRegionFilter(region string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	if region == "" {
		return false
	}

	included := false
	hasInclude := false

	for _, filter := range filters {
		if len(filter) > 0 && filter[0] == '!' {
			if region == filter[1:] {
				return false
			}
			continue
		}
		hasInclude = true
		if region == filter {
			included = true
		}
	}

	if hasInclude && !included {
		return false
	}
	return true
}
