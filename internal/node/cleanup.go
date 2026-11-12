package node

import (
	"sync"
	"time"
)

const (
	HighLatencyCleanupMinSamples   = 3
	HighLatencyCleanupMinSpan      = 10 * time.Minute
	HighLatencyCleanupMaxSampleAge = 2 * time.Minute
)

// HighLatencyCleanupPolicy is an immutable snapshot of the cleanup settings.
// Revision changes when runtime configuration is replaced, invalidating evidence.
type HighLatencyCleanupPolicy struct {
	Enabled     bool
	ThresholdMS int
	Authorities []string
	Revision    uint64
}

type highLatencyEvidence struct {
	mu          sync.Mutex
	revision    uint64
	thresholdMS int
	firstHigh   time.Time
	lastSample  time.Time
	samples     int
}

// ObserveHighLatencySample is called only for new authority-domain measurements.
// Both the raw sample and the reference EWMA must exceed the threshold. In
// particular, a fast raw sample resets the streak even while the EWMA lags.
func (e *NodeEntry) ObserveHighLatencySample(policy HighLatencyCleanupPolicy, latency *time.Duration, referenceHigh bool, at time.Time) {
	if e == nil {
		return
	}
	s := &e.highLatencyEvidence
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revision != policy.Revision || s.thresholdMS != policy.ThresholdMS {
		s.revision, s.thresholdMS = policy.Revision, policy.ThresholdMS
		s.firstHigh, s.lastSample, s.samples = time.Time{}, time.Time{}, 0
	}
	if !policy.Enabled || policy.ThresholdMS <= 0 || latency == nil || *latency <= 0 ||
		float64(*latency)/float64(time.Millisecond) <= float64(policy.ThresholdMS) || !referenceHigh {
		s.firstHigh, s.samples = time.Time{}, 0
		if at.After(s.lastSample) {
			s.lastSample = at
		}
		return
	}
	// Duplicate/out-of-order high readings cannot add evidence. Low readings
	// above deliberately reset even when delivered out of order.
	if !at.After(s.lastSample) {
		return
	}
	if s.samples == 0 {
		s.firstHigh = at
	}
	if s.samples < HighLatencyCleanupMinSamples {
		s.samples++
	}
	s.lastSample = at
}

// ResetHighLatencyEvidence interrupts the streak when a health request fails.
func (e *NodeEntry) ResetHighLatencyEvidence() {
	e.ResetHighLatencyEvidenceAt(time.Time{})
}

// ResetHighLatencyEvidenceAt also rejects high readings delivered late from
// before the failure, so concurrent callbacks cannot revive an interrupted run.
func (e *NodeEntry) ResetHighLatencyEvidenceAt(at time.Time) {
	if e == nil {
		return
	}
	s := &e.highLatencyEvidence
	s.mu.Lock()
	defer s.mu.Unlock()
	s.firstHigh, s.samples = time.Time{}, 0
	if at.After(s.lastSample) {
		s.lastSample = at
	}
}

// ReadyForHighLatencyCleanup never counts cleaner sweeps as new measurements.
// Sample span, rather than elapsed wall time alone, must cover the full window.
func (e *NodeEntry) ReadyForHighLatencyCleanup(policy HighLatencyCleanupPolicy, now time.Time) bool {
	if e == nil {
		return false
	}
	if !policy.Enabled || !ExceedsReferenceLatency(e, policy.Authorities, policy.ThresholdMS) {
		e.ResetHighLatencyEvidence()
		return false
	}
	s := &e.highLatencyEvidence
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision == policy.Revision && s.thresholdMS == policy.ThresholdMS &&
		s.samples >= HighLatencyCleanupMinSamples &&
		s.lastSample.Sub(s.firstHigh) >= HighLatencyCleanupMinSpan &&
		!now.Before(s.lastSample) && now.Sub(s.lastSample) <= HighLatencyCleanupMaxSampleAge
}

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
