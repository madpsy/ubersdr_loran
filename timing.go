// timing.go — UTC timing from PCM wall-clock timestamps
//
// The UberSDR PCM binary packet format includes a wall-clock timestamp in
// bytes [12:20] of the full header (uint64, milliseconds since Unix epoch).
// This is the server's system clock at the time the packet was generated.
//
// TimingInfo exposes this timestamp as a UTC time.Time so that the frontend
// can display the UTC time of the most recent Loran-C pulse measurement.
//
// Note: this is the server's wall clock, not GPS-disciplined time.
// For GPS-disciplined timing, the ka9q-radio radiod STATUS packets carry
// GPSTimeNs (GPS-synchronized Unix time in nanoseconds) — but that requires
// a separate multicast UDP listener which is out of scope for this tool.
// The wall-clock accuracy is typically ±1–10 ms (NTP-disciplined server).

package main

import (
	"sync/atomic"
	"time"
)

// TimingInfo holds the most recent wall-clock timing information derived
// from the PCM packet header.
type TimingInfo struct {
	WallClockMs uint64    `json:"wall_clock_ms"` // raw uint64 from PCM header
	UTC         time.Time `json:"utc"`           // parsed as UTC time
	AgeMs       int64     `json:"age_ms"`        // milliseconds since this timestamp was received
	Valid       bool      `json:"valid"`         // false if no packet received yet
}

// GetTimingInfo returns the current timing information derived from the most
// recent PCM packet wall-clock timestamp stored in the ScopeServer.
func (s *ScopeServer) GetTimingInfo() TimingInfo {
	ms := atomic.LoadUint64(&s.wallClockMs)
	if ms == 0 {
		return TimingInfo{Valid: false}
	}
	t := time.UnixMilli(int64(ms)).UTC()
	age := time.Since(t).Milliseconds()
	return TimingInfo{
		WallClockMs: ms,
		UTC:         t,
		AgeMs:       age,
		Valid:       true,
	}
}
