// decoder.go — Loran-C GRI pulse-envelope decoder
//
// Ported line-by-line from KiwiSDR extensions/Loran_C/loran_c.cpp
// Original copyright (c) 2016 John Seamons, ZL4VO/KF6VO
// Go port copyright (c) 2026 UberSDR project

package main

import (
	"math"
	"sort"
	"sync"
)

// ---------------------------------------------------------------------------
// Constants — mirror the #defines in loran_c.cpp
// ---------------------------------------------------------------------------

const (
	// cmdScopeData / cmdScopeReset mirror SCOPE_DATA / SCOPE_RESET.
	cmdScopeData  = 0
	cmdScopeReset = 1

	// sndRateHalfThreshold mirrors SND_RATE_HALF_THRESHOLD.
	sndRateHalfThreshold = 12000

	// maxGRI mirrors MAX_GRI.
	maxGRI = 9999

	// maxBucket mirrors MAX_BUCKET = (int)(SND_RATE_HALF_THRESHOLD * GRI_2_SEC(MAX_GRI))
	//   = (int)(12000 * 9999/1e5) = (int)(1199.88) = 1199
	maxBucket = 1199

	// cs16Max mirrors CUTESDR_MAX_VAL — maximum magnitude of a CS16 int16 IQ sample.
	cs16Max = float32(32767.0)

	// avgCMA / avgEMA / avgIIR mirror AVG_CMA / AVG_EMA / AVG_IIR.
	avgCMA = 0
	avgEMA = 1
	avgIIR = 2

	// nch is the number of simultaneous GRI channels — one per ChainDB entry.
	// Defined as a constant so it can be used in array bounds at compile time;
	// a build-time check in init() ensures it matches len(ChainDB).
	nch = 14
)

func init() {
	if len(ChainDB) != nch {
		panic("nch constant does not match len(ChainDB) — update nch in decoder.go")
	}
}

// ---------------------------------------------------------------------------
// loranChannel mirrors loran_c_ch_t
// ---------------------------------------------------------------------------

type loranChannel struct {
	// u4_t gri, samp, nbucket, dsp_samps, avg_samps, navgs
	gri      uint32
	samp     uint32 // wraps at 2^32 (~119 h at 10 kHz) — matches u4_t
	nbucket  uint32
	dspSamps uint32
	avgSamps uint32
	navgs    uint32 // NOTE: starts at ^uint32(0) (i.e. -1 cast to uint32) on CMA reset

	// double samp_per_GRI
	sampPerGRI float64

	// float avg[MAX_BUCKET]
	avg [maxBucket]float32

	// float gain, max
	gain float32
	max  float32

	// int offset, avg_algo
	offset  int
	avgAlgo int

	// double avg_param_f; int avg_param_i
	avgParamF float64
	avgParamI int

	// bool restart
	restart bool
}

// ---------------------------------------------------------------------------
// LoranDecoder mirrors loran_c_t (single receiver channel instance)
// ---------------------------------------------------------------------------

type LoranDecoder struct {
	// u4_t i_srate; double srate
	iSrate uint32
	srate  float64

	// scopeThreshold is the dspSamps value that triggers a scope update.
	// KiwiSDR uses i_srate (1 Hz). We default to i_srate/10 (10 Hz) for
	// a more responsive display.
	scopeThreshold uint32

	// loran_c_ch_t ch[NCH]
	ch []loranChannel

	// u1_t scope[MAX_BUCKET]
	scope [maxBucket]byte

	// bool redraw_legend
	redrawLegend bool

	mu sync.Mutex
}

// ---------------------------------------------------------------------------
// Constructor
// ---------------------------------------------------------------------------

// NewLoranDecoder creates a decoder for the given IQ sample rate.
// Mirrors the SET ext_server_init handler in loran_c_msgs().
// updateHz controls how many times per second the scope is sent to the browser.
// The KiwiSDR uses 1 Hz; we default to 10 Hz for a more responsive display.
func NewLoranDecoder(srate float64, updateHz int) *LoranDecoder {
	if updateHz <= 0 {
		updateHz = 10
	}
	iSrate := uint32(srate)
	threshold := iSrate / uint32(updateHz)
	if threshold == 0 {
		threshold = 1
	}
	d := &LoranDecoder{
		srate:          srate,
		iSrate:         iSrate,
		scopeThreshold: threshold,
		ch:             make([]loranChannel, nch),
	}
	// Default averaging parameters (EMA, decay=256) — matches JS defaults.
	for i := 0; i < nch; i++ {
		d.ch[i].avgAlgo = avgEMA
		d.ch[i].avgParamF = 256
		d.ch[i].avgParamI = 256
		d.ch[i].restart = true
	}
	return d
}

// ---------------------------------------------------------------------------
// Parameter setters — mirror the SET handlers in loran_c_msgs()
// ---------------------------------------------------------------------------

// SetGRI mirrors: sscanf(msg, "SET gri%d=%d", &ch, &i_gri)
// init_gri() + redraw_legend=true + restart=true
func (d *LoranDecoder) SetGRI(ch int, gri uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.initGRI(ch, gri)
	d.redrawLegend = true
	d.ch[ch].restart = true
}

// SetOffset mirrors: sscanf(msg, "SET offset%d=%d", &ch, &offset)
// c->offset = (c->offset + offset) % c->nbucket; c->restart = true
//
// In C++, c->offset is int and c->nbucket is u4_t.  The expression
// (c->offset + offset) % c->nbucket converts the left side to u4_t before
// the modulo, so the result is always in [0, nbucket).  We replicate this
// by converting to uint32 before the modulo and back to int.
func (d *LoranDecoder) SetOffset(ch int, delta int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := &d.ch[ch]
	if c.nbucket > 0 {
		// Match C++ unsigned modulo: (uint32)(c->offset + offset) % c->nbucket
		sum := uint32(c.offset + delta)
		c.offset = int(sum % c.nbucket)
	}
	c.restart = true
}

// SetGain mirrors: sscanf(msg, "SET gain%d=%d", &ch, &gain)
// c->gain = gain? pow(10.0, ((float)-gain)/10.0) : 0; c->restart = true
func (d *LoranDecoder) SetGain(ch int, gainDB int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := &d.ch[ch]
	if gainDB == 0 {
		c.gain = 0
	} else {
		c.gain = float32(math.Pow(10.0, float64(-gainDB)/10.0))
	}
	c.restart = true
}

// SetAvgAlgo mirrors: sscanf(msg, "SET avg_algo%d=%d", &ch, &avg_algo)
// c->avg_algo = avg_algo; c->restart = true
func (d *LoranDecoder) SetAvgAlgo(ch int, algo int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ch[ch].avgAlgo = algo
	d.ch[ch].restart = true
}

// SetAvgParam mirrors: sscanf(msg, "SET avg_param%d=%lf", &ch, &avg_param)
// c->avg_param_f = avg_param; c->avg_param_i = lround(avg_param); c->restart = true
func (d *LoranDecoder) SetAvgParam(ch int, param float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := &d.ch[ch]
	c.avgParamF = param
	c.avgParamI = int(math.Round(param))
	c.restart = true
}

// Start mirrors: SET start — redraw_legend=true, all channels restart=true.
func (d *LoranDecoder) Start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.redrawLegend = true
	for i := range d.ch {
		d.ch[i].restart = true
	}
}

// ---------------------------------------------------------------------------
// initGRI mirrors init_gri() in loran_c.cpp
// Must be called with d.mu held.
// ---------------------------------------------------------------------------

func (d *LoranDecoder) initGRI(ch int, gri uint32) {
	c := &d.ch[ch]
	c.gri = gri
	// c->samp_per_GRI = e->srate * GRI_2_SEC(gri)  where GRI_2_SEC(gri) = gri/1e5
	c.sampPerGRI = d.srate * float64(gri) / 1e5
	// c->nbucket = floor(c->samp_per_GRI) + 1
	c.nbucket = uint32(math.Floor(c.sampPerGRI)) + 1
	// if (c->nbucket > MAX_BUCKET) { c->samp_per_GRI /= 2; c->nbucket /= 2; }
	if c.nbucket > maxBucket {
		c.sampPerGRI /= 2
		c.nbucket /= 2
	}
}

// ---------------------------------------------------------------------------
// MsPerBin mirrors the ms_per_bin calculation in loran_c_msgs SET ext_server_init.
//
//	float ms_per_bin = 1.0/e->srate * 1e3;
//	if (snd_rate > SND_RATE_HALF_THRESHOLD) ms_per_bin *= 2;
//
// ---------------------------------------------------------------------------

func (d *LoranDecoder) MsPerBin() float64 {
	msPerBin := 1.0 / d.srate * 1e3
	if d.iSrate > sndRateHalfThreshold {
		msPerBin *= 2
	}
	return msPerBin
}

// ---------------------------------------------------------------------------
// ChannelQuality holds per-channel signal quality metrics derived from the
// averaged power envelope.  Computed once per scope frame.
// ---------------------------------------------------------------------------

type ChannelQuality struct {
	ChIdx    int     `json:"ch_idx"`    // channel index (0 … nch-1)
	GRI      uint32  `json:"gri"`       // GRI value for this channel
	PeakBin  int     `json:"peak_bin"`  // bucket index of the highest averaged power
	PeakPwr  float32 `json:"peak_pwr"`  // averaged power at PeakBin (raw float32 units)
	NoisePwr float32 `json:"noise_pwr"` // mean power of the lowest 50% of buckets (noise floor estimate)
	SNR      float32 `json:"snr_db"`    // 10·log10(PeakPwr/NoisePwr) in dB; 0 if NoisePwr==0
	Navgs    uint32  `json:"navgs"`     // number of GRI averages accumulated (CMA only; 0 for EMA/IIR)
}

// GetAvg returns a copy of the averaged power envelope for channel ch.
// The returned slice has length c.nbucket (may be 0 if GRI not yet set).
// Safe to call from any goroutine.
func (d *LoranDecoder) GetAvg(ch int) []float32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := &d.ch[ch]
	if c.nbucket == 0 {
		return nil
	}
	out := make([]float32, c.nbucket)
	copy(out, c.avg[:c.nbucket])
	return out
}

// Quality computes signal quality metrics for channel ch from the current
// averaged envelope.  Returns zero-value ChannelQuality if GRI not set.
// Safe to call from any goroutine.
func (d *LoranDecoder) Quality(ch int) ChannelQuality {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := &d.ch[ch]
	if c.nbucket == 0 || c.gri == 0 {
		return ChannelQuality{}
	}

	n := int(c.nbucket)
	// Find peak bin and collect all values.
	peakBin := 0
	peakPwr := float32(0)
	vals := make([]float32, n)
	for j := 0; j < n; j++ {
		vals[j] = c.avg[j]
		if c.avg[j] > peakPwr {
			peakPwr = c.avg[j]
			peakBin = j
		}
	}

	// Noise floor: mean of the lower 50% of bucket values (sorted ascending).
	sorted := make([]float32, n)
	copy(sorted, vals)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	half := n / 2
	if half < 1 {
		half = 1
	}
	var noiseSum float32
	for j := 0; j < half; j++ {
		noiseSum += sorted[j]
	}
	noisePwr := noiseSum / float32(half)

	var snr float32
	if noisePwr > 0 && peakPwr > 0 {
		snr = float32(10.0 * math.Log10(float64(peakPwr/noisePwr)))
	}

	navgs := uint32(0)
	if c.avgAlgo == avgCMA {
		navgs = c.navgs
	}

	return ChannelQuality{
		ChIdx:    ch,
		GRI:      c.gri,
		PeakBin:  peakBin,
		PeakPwr:  peakPwr,
		NoisePwr: noisePwr,
		SNR:      snr,
		Navgs:    navgs,
	}
}

// ---------------------------------------------------------------------------
// ProcessSamples mirrors loran_c_data() in loran_c.cpp
//
// samples is a slice of interleaved CS16 IQ pairs: [I0, Q0, I1, Q1, …]
//
// fn is called whenever a complete scope frame is ready for one channel:
//
//	fn(ch int, cmd byte, payload []byte)
//
// payload matches the wire format sent by ext_send_msg_data():
//
//	payload[0]   = ch  (channel index, 0 or 1)
//	payload[1..] = scope bytes (nbucket values, 0-255)
//
// cmd is cmdScopeData or cmdScopeReset.
// ---------------------------------------------------------------------------

func (d *LoranDecoder) ProcessSamples(samples []int16, fn func(ch int, cmd byte, payload []byte)) {
	nSamps := len(samples) / 2 // each IQ pair is 2 × int16

	// Collect any scope frames to emit after releasing the lock.
	// This avoids holding d.mu while calling fn (which broadcasts to WebSocket clients).
	type pendingFrame struct {
		ch      int
		cmd     byte
		payload []byte
	}
	var pending []pendingFrame

	d.mu.Lock()

	for i := 0; i < nSamps; i++ {
		// float re = (float) samps[i].re;
		// float im = (float) samps[i].im;
		// float pwr = re*re + im*im;
		re := float32(samples[i*2])
		im := float32(samples[i*2+1])
		pwr := re*re + im*im

		for ch := 0; ch < nch; ch++ {
			c := &d.ch[ch]

			// Skip channel if GRI not yet configured.
			if c.gri == 0 || c.nbucket == 0 || c.sampPerGRI == 0 {
				c.samp++
				continue
			}

			// int bn = floor(fmod(c->samp - c->offset, c->samp_per_GRI));
			//
			// c->samp and c->offset are both u4_t (uint32).
			// The subtraction wraps in unsigned arithmetic, exactly as in C.
			// fmod then gives the fractional position within the GRI period.
			sampMinusOffset := float64(c.samp - uint32(c.offset))
			bn := int(math.Floor(math.Mod(sampMinusOffset, c.sampPerGRI)))

			// if (bn == 0 && c->dsp_samps > e->i_srate) { ... }
			// We use scopeThreshold instead of iSrate to allow faster updates
			// than the KiwiSDR's 1 Hz default (e.g. 10 Hz for a responsive UI).
			if bn == 0 && c.dspSamps > d.scopeThreshold {
				// c->dsp_samps = 0;
				c.dspSamps = 0

				// Determine display maximum.
				if c.gain == 0 {
					// Auto-scale: find peak across all buckets.
					// c->max = 0; for (j=0; j<c->nbucket; j++) if (c->avg[j] > c->max) c->max = c->avg[j];
					c.max = 0
					for j := uint32(0); j < c.nbucket; j++ {
						if c.avg[j] > c.max {
							c.max = c.avg[j]
						}
					}
				} else {
					// c->max = c->gain * CUTESDR_MAX_VAL;
					c.max = c.gain * cs16Max
				}

				// Build scope frame: scope[0]=ch, scope[j+1]=normalised value.
				// e->scope[j+1] = scope = c->max? (255*(avg/c->max)) : 0
				// ext_send_msg_data sends e->scope with length c->nbucket+1
				payload := make([]byte, c.nbucket+1)
				payload[0] = byte(ch)
				for j := uint32(0); j < c.nbucket; j++ {
					avg := c.avg[j]
					if avg > c.max {
						avg = c.max
					}
					if avg < 0 {
						avg = 0
					}
					var s int
					if c.max != 0 {
						s = int(255 * (avg / c.max))
					}
					payload[j+1] = byte(s)
				}

				// e->redraw_legend? SCOPE_RESET : SCOPE_DATA
				// e->redraw_legend = false;
				cmd := byte(cmdScopeData)
				if d.redrawLegend {
					cmd = cmdScopeReset
					d.redrawLegend = false
				}

				pending = append(pending, pendingFrame{ch: ch, cmd: cmd, payload: payload})
			}

			// c->dsp_samps++;  (always, after the scope-send block)
			c.dspSamps++

			// ---------------------------------------------------------------------------
			// Averaging — exactly mirrors the if/else chain in loran_c_data()
			// ---------------------------------------------------------------------------

			if c.avgAlgo == avgCMA {
				// if (bn == 0 && (c->restart || (c->avg_samps > (e->i_srate * c->avg_param_i))))
				if bn == 0 && (c.restart || (c.avgSamps > d.iSrate*uint32(c.avgParamI))) {
					// if (c->restart) { c->restart = false; c->dsp_samps = 0; }
					if c.restart {
						c.restart = false
						c.dspSamps = 0
					}
					// for (j=0; j<c->nbucket; j++) c->avg[j] = 0;
					for j := uint32(0); j < c.nbucket; j++ {
						c.avg[j] = 0
					}
					// c->avg_samps = 0; c->navgs = -1;
					c.avgSamps = 0
					c.navgs = ^uint32(0) // -1 as uint32 (wraps to 0xFFFFFFFF)
				}
				// c->avg_samps++;
				c.avgSamps++

				// if (bn == 0) c->navgs++;
				if bn == 0 {
					c.navgs++ // wraps from 0xFFFFFFFF → 0 on first increment
				}

				// if (bn < c->nbucket-1) {
				//   c->avg[bn] = (c->avg[bn] * c->navgs) + pwr;
				//   c->avg[bn] /= c->navgs + 1;
				// }
				if bn < int(c.nbucket)-1 {
					n := float32(c.navgs)
					c.avg[bn] = (c.avg[bn]*n + pwr) / (n + 1)
				}

			} else if c.avgAlgo == avgEMA {
				// if (bn == 0 && c->restart) { c->restart=false; c->dsp_samps=0; zero avg[]; }
				if bn == 0 && c.restart {
					c.restart = false
					c.dspSamps = 0
					for j := uint32(0); j < c.nbucket; j++ {
						c.avg[j] = 0
					}
				}
				// if (bn < c->nbucket-1) { c->avg[bn] += (pwr - c->avg[bn]) / DECAY; }
				// where DECAY = c->avg_param_i
				if bn < int(c.nbucket)-1 {
					decay := float32(c.avgParamI)
					if decay < 1 {
						decay = 1
					}
					c.avg[bn] += (pwr - c.avg[bn]) / decay
				}

			} else if c.avgAlgo == avgIIR {
				// if (bn == 0 && c->restart) { c->restart=false; c->dsp_samps=0; zero avg[]; }
				if bn == 0 && c.restart {
					c.restart = false
					c.dspSamps = 0
					for j := uint32(0); j < c.nbucket; j++ {
						c.avg[j] = 0
					}
				}
				// if (bn < c->nbucket-1) {
				//   float iir_gain = 1.0 - expf(-(c->avg_param_f) * pwr/CUTESDR_MAX_VAL);
				//   c->avg[bn] += (pwr - c->avg[bn]) * iir_gain;
				// }
				if bn < int(c.nbucket)-1 {
					iirGain := float32(1.0 - math.Exp(-c.avgParamF*float64(pwr)/float64(cs16Max)))
					c.avg[bn] += (pwr - c.avg[bn]) * iirGain
				}
			}
			// (no else panic — unknown algo is silently ignored in production)

			// c->samp++;
			c.samp++
		}
	}

	d.mu.Unlock()

	// Emit pending scope frames outside the lock so WebSocket sends don't
	// block the decoder.
	for _, f := range pending {
		fn(f.ch, f.cmd, f.payload)
	}
}
