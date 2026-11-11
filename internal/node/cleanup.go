package node

// IsFailedForCleanup excludes nodes awaiting their first probe.
func IsFailedForCleanup(entry *NodeEntry) bool {
	return entry != nil && ((!entry.HasOutbound() && entry.GetLastError() != "") ||
		(entry.IsCircuitOpen() && entry.FailureCount.Load() > 0))
}

// ExceedsReferenceLatency uses the same authority-domain average as the node list.
// Unknown latency and circuit-open nodes do not qualify as high-latency nodes.
func ExceedsReferenceLatency(entry *NodeEntry, authorities []string, thresholdMS int) bool {
	if entry == nil || thresholdMS <= 0 || !entry.HasOutbound() || entry.IsCircuitOpen() {
		return false
	}
	latencyMS, ok := AverageEWMAForDomainsMs(entry, authorities)
	return ok && latencyMS > float64(thresholdMS)
}
