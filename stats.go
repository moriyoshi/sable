package sable

// stats.go — R6a observability. A snapshot of the Rust runtime's counters,
// suitable for exporting to a metrics system (Prometheus, statsd) or a pprof/
// expvar endpoint. Core — works under both the fast and portable backends.

// #include "sable.h"
import "C"

// Stats is a point-in-time snapshot of runtime counters. Totals are monotonic;
// InFlight, QueueDepth and CancelsRegistered are gauges.
type Stats struct {
	Spawned           uint64 // tasks admitted (one per spawn entry point)
	Completed         uint64 // completions delivered (results + cancellations)
	Cancelled         uint64 // of Completed, the cancellation deliveries
	InFlight          uint64 // gauge: Spawned - Completed
	QueueDepth        uint64 // gauge: completions queued for the dispatcher
	PeakQueueDepth    uint64 // high-water mark of QueueDepth
	CancelsRegistered uint64 // cancellable calls currently registered
	Rejected          uint64 // admissions refused at the in-flight cap (TryCall)
	MaxInFlight       uint64 // current in-flight cap (0 = unbounded)
}

// RuntimeStats returns a metrics snapshot of the Rust runtime.
func RuntimeStats() Stats {
	Init()
	var c C.SableStats
	C.sable_stats(rt, &c)
	return Stats{
		Spawned:           uint64(c.spawned),
		Completed:         uint64(c.completed),
		Cancelled:         uint64(c.cancelled),
		InFlight:          uint64(c.in_flight),
		QueueDepth:        uint64(c.queue_depth),
		PeakQueueDepth:    uint64(c.peak_queue_depth),
		CancelsRegistered: uint64(c.cancels_registered),
		Rejected:          uint64(c.rejected),
		MaxInFlight:       uint64(c.max_in_flight),
	}
}
