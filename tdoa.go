// tdoa.go — Time Difference of Arrival measurement engine for Loran-C
//
// For each GRI chain, measures the time delay between the master station
// pulse and each secondary station pulse using parabolic interpolation on
// the averaged power envelope.  Subtracts the known emission delay to
// produce a TDOA value in microseconds.
//
// The sample rate (and therefore µs-per-bin) is NOT hardcoded here.
// It is read from the actual PCM packet header delivered by UberSDR and
// passed in via the decoder's MsPerBin() method.  This means the engine
// works correctly regardless of which IQ mode is in use:
//
//   "iq"    → ±5 kHz bandwidth  → 10,000 Hz sample rate → 100 µs/bin
//   "iq48"  → ±24 kHz bandwidth → 48,000 Hz sample rate → ~20.8 µs/bin
//   "iq96"  → ±48 kHz bandwidth → 96,000 Hz sample rate → ~10.4 µs/bin
//   "iq192" → ±96 kHz bandwidth → 192,000 Hz sample rate → ~5.2 µs/bin
//   "iq384" → ±192 kHz bandwidth → 384,000 Hz sample rate → ~2.6 µs/bin
//
// Accuracy after parabolic interpolation:
//   At 10 kHz:  ~2–10 µs → ~0.6–3 km position accuracy
//   At 48 kHz:  ~0.4–2 µs → ~0.1–0.6 km position accuracy
//   At 192 kHz: ~0.1–0.5 µs → ~30–150 m position accuracy
//
// Propagation speed: 299,700 km/s (Loran-C ground-wave over seawater, ITU-R TF.460)

package main

import (
	"math"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Propagation speed
// ---------------------------------------------------------------------------

const (
	// speedOfLightKmS is the free-space speed of light in km/s.
	speedOfLightKmS = 299792.458

	// loranPropSpeedKmS is the effective propagation speed of Loran-C
	// ground-wave signals over seawater (slightly slower than free-space).
	// ITU-R TF.460 / Loran-C Signal Specification value.
	loranPropSpeedKmS = 299700.0

	// minSNRdB is the minimum SNR (dB) required before a peak is considered
	// valid for TDOA measurement.  Below this threshold the measurement is
	// marked invalid.
	minSNRdB = 3.0
)

// ---------------------------------------------------------------------------
// TDOAMeasurement — result for one secondary station
// ---------------------------------------------------------------------------

// TDOAMeasurement holds the TDOA result for one master→secondary pair.
type TDOAMeasurement struct {
	ChainGRI    uint32    `json:"chain_gri"`
	ChainName   string    `json:"chain_name"`
	MasterID    string    `json:"master_id"`
	SecondaryID string    `json:"secondary_id"`
	EmissionUS  float64   `json:"emission_us"` // known emission delay in µs (from ChainDB)
	MeasuredUS  float64   `json:"measured_us"` // measured delay from master peak in µs
	TDOA_US     float64   `json:"tdoa_us"`     // TDOA = MeasuredUS − EmissionUS (µs)
	PeakBin     int       `json:"peak_bin"`    // bucket index of the secondary peak
	SubBin      float64   `json:"sub_bin"`     // sub-bin offset from parabolic interpolation (−0.5…+0.5)
	SNR         float32   `json:"snr_db"`      // SNR at the secondary peak (dB)
	Valid       bool      `json:"valid"`       // false if SNR too low or peak not found
	UpdatedAt   time.Time `json:"updated_at"`
}

// ---------------------------------------------------------------------------
// TDOAEngine — runs TDOA measurement on all chains
// ---------------------------------------------------------------------------

// TDOAEngine periodically reads the averaged envelopes from a LoranDecoder,
// detects peaks for each station, and computes TDOA values.
// The µs-per-bin value is derived from the decoder's actual sample rate
// (read from the PCM packet header), not from any hardcoded constant.
type TDOAEngine struct {
	dec *LoranDecoder

	mu      sync.RWMutex
	results []TDOAMeasurement // latest results, one per secondary station across all chains
}

// NewTDOAEngine creates a TDOAEngine attached to the given decoder.
func NewTDOAEngine(dec *LoranDecoder) *TDOAEngine {
	return &TDOAEngine{dec: dec}
}

// Update recomputes TDOA measurements for all chains.
// Called after each scope frame (or on demand).
// msPerBin is obtained from dec.MsPerBin() which uses the actual sample rate
// from the PCM packet header — no hardcoded assumptions about IQ mode.
func (e *TDOAEngine) Update() {
	// Get µs-per-bin from the decoder's actual sample rate.
	msPerBin := e.dec.MsPerBin()
	if msPerBin <= 0 {
		return // decoder not yet initialised
	}
	usPerBin := msPerBin * 1000.0 // µs per bin

	now := time.Now()
	var results []TDOAMeasurement

	for chIdx, chain := range ChainDB {
		avg := e.dec.GetAvg(chIdx)
		if len(avg) == 0 {
			continue
		}
		qual := e.dec.Quality(chIdx)
		if qual.GRI == 0 {
			continue
		}

		// The master station is always the first entry in chain.Stations.
		// Subsequent entries are secondaries with known emission delays.
		if len(chain.Stations) < 2 {
			continue
		}

		// Find master peak bin.
		masterStation := chain.Stations[0]
		masterBin, masterSub, masterSNR := findPeak(avg, 0, len(avg))
		masterValid := masterSNR >= minSNRdB

		// For each secondary, find its peak in the window starting at
		// emissionDelay/usPerBin bins after the master peak.
		for _, sec := range chain.Stations[1:] {
			// Expected secondary bin = masterBin + emissionDelay/usPerBin
			expectedBin := masterBin + int(math.Round(sec.DelayUS/usPerBin))

			// Search window: ±10% of emission delay (min ±5 bins, max ±50 bins)
			window := int(math.Round(sec.DelayUS / usPerBin * 0.10))
			if window < 5 {
				window = 5
			}
			if window > 50 {
				window = 50
			}

			lo := expectedBin - window
			hi := expectedBin + window + 1
			if lo < 0 {
				lo = 0
			}
			if hi > len(avg) {
				hi = len(avg)
			}

			secBin, secSub, secSNR := findPeak(avg, lo, hi)
			secValid := masterValid && secSNR >= minSNRdB

			// Measured delay from master to secondary in µs.
			measuredUS := float64(secBin-masterBin)*usPerBin +
				(secSub-masterSub)*usPerBin

			tdoa := measuredUS - sec.DelayUS

			results = append(results, TDOAMeasurement{
				ChainGRI:    chain.GRI,
				ChainName:   chain.Name,
				MasterID:    masterStation.ID,
				SecondaryID: sec.ID,
				EmissionUS:  sec.DelayUS,
				MeasuredUS:  measuredUS,
				TDOA_US:     tdoa,
				PeakBin:     secBin,
				SubBin:      secSub,
				SNR:         secSNR,
				Valid:       secValid,
				UpdatedAt:   now,
			})
		}
		_ = qual // used for future quality gating
		_ = masterStation
	}

	e.mu.Lock()
	e.results = results
	e.mu.Unlock()
}

// Results returns a copy of the latest TDOA measurements.
func (e *TDOAEngine) Results() []TDOAMeasurement {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]TDOAMeasurement, len(e.results))
	copy(out, e.results)
	return out
}

// ---------------------------------------------------------------------------
// findPeak — parabolic interpolation peak detector
// ---------------------------------------------------------------------------

// findPeak finds the bin with maximum power in avg[lo:hi] and applies
// parabolic interpolation to estimate the sub-bin offset.
//
// Returns:
//
//	bin    — integer bin index of the peak (absolute, not relative to lo)
//	subBin — sub-bin offset in [−0.5, +0.5] from parabolic fit
//	snr    — estimated SNR in dB (peak vs. local noise floor of the window)
func findPeak(avg []float32, lo, hi int) (bin int, subBin float64, snr float32) {
	if hi <= lo || lo < 0 || hi > len(avg) {
		return lo, 0, 0
	}

	// Find maximum in window.
	peakBin := lo
	peakVal := avg[lo]
	for i := lo + 1; i < hi; i++ {
		if avg[i] > peakVal {
			peakVal = avg[i]
			peakBin = i
		}
	}

	// Parabolic interpolation using the three bins around the peak.
	// δ = (y[n+1] − y[n−1]) / (2·(2·y[n] − y[n−1] − y[n+1]))
	sub := 0.0
	if peakBin > lo && peakBin < hi-1 {
		ym1 := float64(avg[peakBin-1])
		y0 := float64(avg[peakBin])
		yp1 := float64(avg[peakBin+1])
		denom := 2.0 * (2.0*y0 - ym1 - yp1)
		if math.Abs(denom) > 1e-12 {
			sub = (yp1 - ym1) / denom
			// Clamp to ±0.5 bins.
			if sub > 0.5 {
				sub = 0.5
			} else if sub < -0.5 {
				sub = -0.5
			}
		}
	}

	// SNR estimate: peak vs. mean of the lower 50% of the window.
	n := hi - lo
	sorted := make([]float32, n)
	copy(sorted, avg[lo:hi])
	// Insertion sort (n ≤ ~100 for typical windows).
	for i := 1; i < n; i++ {
		key := sorted[i]
		j := i - 1
		for j >= 0 && sorted[j] > key {
			sorted[j+1] = sorted[j]
			j--
		}
		sorted[j+1] = key
	}
	half := n / 2
	if half < 1 {
		half = 1
	}
	var noiseSum float32
	for i := 0; i < half; i++ {
		noiseSum += sorted[i]
	}
	noisePwr := noiseSum / float32(half)

	var snrDB float32
	if noisePwr > 0 && peakVal > 0 {
		snrDB = float32(10.0 * math.Log10(float64(peakVal/noisePwr)))
	}

	return peakBin, sub, snrDB
}
