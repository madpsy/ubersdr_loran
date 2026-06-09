// lop.go — Line-of-Position computation and Gauss-Newton position fix
//
// Implements:
//   1. Vincenty inverse formula (WGS-84) for geodetic distance and bearing
//   2. Vincenty direct formula for computing a point at distance+bearing
//   3. Hyperbolic LOP: the set of points where
//        d(P, Master) − d(P, Secondary) = c_prop × TDOA_us × 1e-6  km
//   4. Gauss-Newton least-squares position fix from multiple LOPs
//
// References:
//   Vincenty, T. (1975). "Direct and Inverse Solutions of Geodesics on the
//     Ellipsoid with Application of Nested Equations". Survey Review.
//   Loran-C Signal Specification, US Coast Guard, 1994.

package main

import (
	"math"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// WGS-84 ellipsoid constants
// ---------------------------------------------------------------------------

const (
	wgs84A = 6378137.0             // semi-major axis (m)
	wgs84F = 1.0 / 298.257223563   // flattening
	wgs84B = wgs84A * (1 - wgs84F) // semi-minor axis (m)
)

// ---------------------------------------------------------------------------
// LatLon — a WGS-84 geodetic coordinate
// ---------------------------------------------------------------------------

// LatLon holds a WGS-84 geodetic coordinate in decimal degrees.
type LatLon struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// ---------------------------------------------------------------------------
// Vincenty inverse — distance and forward azimuth between two points
// ---------------------------------------------------------------------------

// vincentyInverse computes the geodetic distance (metres) and forward azimuth
// (radians, clockwise from north) between two WGS-84 points using Vincenty's
// inverse formula.  Returns (0, 0) for coincident points.
func vincentyInverse(p1, p2 LatLon) (distM, az1Rad float64) {
	φ1 := p1.Lat * math.Pi / 180
	φ2 := p2.Lat * math.Pi / 180
	L := (p2.Lon - p1.Lon) * math.Pi / 180

	U1 := math.Atan((1 - wgs84F) * math.Tan(φ1))
	U2 := math.Atan((1 - wgs84F) * math.Tan(φ2))
	sinU1, cosU1 := math.Sin(U1), math.Cos(U1)
	sinU2, cosU2 := math.Sin(U2), math.Cos(U2)

	λ := L
	var sinλ, cosλ float64
	var sinσ, cosσ, σ, sinα, cos2α, cos2σm float64

	for iter := 0; iter < 100; iter++ {
		sinλ = math.Sin(λ)
		cosλ = math.Cos(λ)

		t1 := cosU2 * sinλ
		t2 := cosU1*sinU2 - sinU1*cosU2*cosλ
		sinσ = math.Sqrt(t1*t1 + t2*t2)
		if sinσ == 0 {
			return 0, 0 // coincident points
		}
		cosσ = sinU1*sinU2 + cosU1*cosU2*cosλ
		σ = math.Atan2(sinσ, cosσ)
		sinα = cosU1 * cosU2 * sinλ / sinσ
		cos2α = 1 - sinα*sinα
		if cos2α == 0 {
			cos2σm = 0 // equatorial line
		} else {
			cos2σm = cosσ - 2*sinU1*sinU2/cos2α
		}
		C := wgs84F / 16 * cos2α * (4 + wgs84F*(4-3*cos2α))
		λPrev := λ
		λ = L + (1-C)*wgs84F*sinα*(σ+C*sinσ*(cos2σm+C*cosσ*(-1+2*cos2σm*cos2σm)))
		if math.Abs(λ-λPrev) < 1e-12 {
			break
		}
	}

	u2 := cos2α * (wgs84A*wgs84A - wgs84B*wgs84B) / (wgs84B * wgs84B)
	A := 1 + u2/16384*(4096+u2*(-768+u2*(320-175*u2)))
	B := u2 / 1024 * (256 + u2*(-128+u2*(74-47*u2)))
	Δσ := B * sinσ * (cos2σm + B/4*(cosσ*(-1+2*cos2σm*cos2σm)-
		B/6*cos2σm*(-3+4*sinσ*sinσ)*(-3+4*cos2σm*cos2σm)))

	distM = wgs84B * A * (σ - Δσ)

	az1Rad = math.Atan2(cosU2*sinλ, cosU1*sinU2-sinU1*cosU2*cosλ)
	return distM, az1Rad
}

// vincentyDistKm returns the geodetic distance in kilometres between two points.
func vincentyDistKm(p1, p2 LatLon) float64 {
	d, _ := vincentyInverse(p1, p2)
	return d / 1000.0
}

// ---------------------------------------------------------------------------
// Vincenty direct — compute destination point given start, bearing, distance
// ---------------------------------------------------------------------------

// vincentyDirect computes the destination point given a start point, forward
// azimuth (radians, clockwise from north), and distance (metres).
func vincentyDirect(start LatLon, az1Rad, distM float64) LatLon {
	φ1 := start.Lat * math.Pi / 180
	α1 := az1Rad
	s := distM

	sinα1, cosα1 := math.Sin(α1), math.Cos(α1)
	tanU1 := (1 - wgs84F) * math.Tan(φ1)
	cosU1 := 1 / math.Sqrt(1+tanU1*tanU1)
	sinU1 := tanU1 * cosU1

	σ1 := math.Atan2(tanU1, cosα1)
	sinα := cosU1 * sinα1
	cos2α := 1 - sinα*sinα
	u2 := cos2α * (wgs84A*wgs84A - wgs84B*wgs84B) / (wgs84B * wgs84B)
	A := 1 + u2/16384*(4096+u2*(-768+u2*(320-175*u2)))
	B := u2 / 1024 * (256 + u2*(-128+u2*(74-47*u2)))

	σ := s / (wgs84B * A)
	var sinσ, cosσ, cos2σm float64
	for iter := 0; iter < 100; iter++ {
		cos2σm = math.Cos(2*σ1 + σ)
		sinσ = math.Sin(σ)
		cosσ = math.Cos(σ)
		Δσ := B * sinσ * (cos2σm + B/4*(cosσ*(-1+2*cos2σm*cos2σm)-
			B/6*cos2σm*(-3+4*sinσ*sinσ)*(-3+4*cos2σm*cos2σm)))
		σPrev := σ
		σ = s/(wgs84B*A) + Δσ
		if math.Abs(σ-σPrev) < 1e-12 {
			break
		}
	}

	cos2σm = math.Cos(2*σ1 + σ)
	sinσ = math.Sin(σ)
	cosσ = math.Cos(σ)

	φ2 := math.Atan2(
		sinU1*cosσ+cosU1*sinσ*cosα1,
		(1-wgs84F)*math.Sqrt(sinα*sinα+(sinU1*sinσ-cosU1*cosσ*cosα1)*(sinU1*sinσ-cosU1*cosσ*cosα1)),
	)
	λ := math.Atan2(sinσ*sinα1, cosU1*cosσ-sinU1*sinσ*cosα1)
	C := wgs84F / 16 * cos2α * (4 + wgs84F*(4-3*cos2α))
	L := λ - (1-C)*wgs84F*sinα*(σ+C*sinσ*(cos2σm+C*cosσ*(-1+2*cos2σm*cos2σm)))

	return LatLon{
		Lat: φ2 * 180 / math.Pi,
		Lon: start.Lon + L*180/math.Pi,
	}
}

// ---------------------------------------------------------------------------
// LOP — Line of Position
// ---------------------------------------------------------------------------

// LOP represents a hyperbolic line of position for one master→secondary pair.
// The LOP is the set of points P where:
//
//	d(P, Master) − d(P, Secondary) = c_prop × TDOA_us × 1e-6  km
//
// We store a sampled polyline of the LOP for map display.
type LOP struct {
	ChainGRI    uint32    `json:"chain_gri"`
	ChainName   string    `json:"chain_name"`
	MasterID    string    `json:"master_id"`
	SecondaryID string    `json:"secondary_id"`
	TDOA_US     float64   `json:"tdoa_us"`
	RangeDiffKm float64   `json:"range_diff_km"` // c_prop × TDOA_us × 1e-6
	Valid       bool      `json:"valid"`
	Points      []LatLon  `json:"points"` // sampled polyline (≈ 200 points)
	UpdatedAt   time.Time `json:"updated_at"`
}

// computeLOP traces the hyperbolic LOP for a given TDOA measurement.
// master and secondary are the transmitter positions.
// propSpeedKmS is the propagation speed (use loranPropSpeedKmS = 299700 km/s).
// nPoints is the number of polyline points to generate (typically 200).
func computeLOP(m TDOAMeasurement, master, secondary LatLon, propSpeedKmS float64, nPoints int) LOP {
	lop := LOP{
		ChainGRI:    m.ChainGRI,
		ChainName:   m.ChainName,
		MasterID:    m.MasterID,
		SecondaryID: m.SecondaryID,
		TDOA_US:     m.TDOA_US,
		RangeDiffKm: propSpeedKmS * m.TDOA_US * 1e-6,
		Valid:       m.Valid,
		UpdatedAt:   m.UpdatedAt,
	}
	if !m.Valid {
		return lop
	}

	rangeDiffKm := lop.RangeDiffKm

	// The hyperbola is parameterised by the angle θ swept around the midpoint
	// of the master–secondary baseline.  We sample θ from 0 to 2π and for
	// each θ find the point P on the LOP by Newton-Raphson on:
	//   f(r) = d(P(r,θ), master) − d(P(r,θ), secondary) − rangeDiffKm = 0
	// where P(r,θ) is the point at distance r from the midpoint in direction θ.

	midLat := (master.Lat + secondary.Lat) / 2
	midLon := (master.Lon + secondary.Lon) / 2
	mid := LatLon{Lat: midLat, Lon: midLon}

	baselineKm := vincentyDistKm(master, secondary)
	if baselineKm < 1 {
		return lop // degenerate — stations too close
	}

	// Half-distance between foci (km).
	c := baselineKm / 2
	// Semi-transverse axis (km): a = |rangeDiffKm| / 2
	a := math.Abs(rangeDiffKm) / 2
	if a >= c {
		// |TDOA| too large — point is beyond the baseline; LOP degenerates.
		return lop
	}

	// Bearing from mid to master (radians).
	_, az := vincentyInverse(mid, master)

	if nPoints < 10 {
		nPoints = 200
	}
	points := make([]LatLon, 0, nPoints)

	// Sample the hyperbola in polar coordinates centred on the midpoint.
	// For each angle θ, the hyperbola in standard form (x²/a² − y²/b² = 1)
	// gives r(θ) = b² / (a − c·cos(θ)) for the branch closer to the master.
	b2 := c*c - a*a // b² = c² − a²

	for i := 0; i < nPoints; i++ {
		θ := 2 * math.Pi * float64(i) / float64(nPoints)
		cosθ := math.Cos(θ)
		denom := a - c*cosθ
		if math.Abs(denom) < 1e-9 {
			continue
		}
		r := b2 / denom
		if r < 0 {
			// Other branch of the hyperbola — skip (we only want the branch
			// corresponding to the sign of rangeDiffKm).
			continue
		}

		// Direction: rotate θ by the baseline azimuth.
		bearing := az + θ
		pt := vincentyDirect(mid, bearing, r*1000) // r in metres
		points = append(points, pt)
	}

	lop.Points = points
	return lop
}

// ---------------------------------------------------------------------------
// LOPEngine — computes LOPs from TDOA measurements
// ---------------------------------------------------------------------------

// LOPEngine computes Lines of Position from the latest TDOA measurements.
type LOPEngine struct {
	tdoa *TDOAEngine

	mu   sync.RWMutex
	lops []LOP
}

// NewLOPEngine creates a LOPEngine that reads from the given TDOAEngine.
func NewLOPEngine(tdoa *TDOAEngine) *LOPEngine {
	return &LOPEngine{tdoa: tdoa}
}

// Update recomputes all LOPs from the latest TDOA measurements.
func (e *LOPEngine) Update() {
	measurements := e.tdoa.Results()
	lops := make([]LOP, 0, len(measurements))

	for _, m := range measurements {
		if !m.Valid {
			lops = append(lops, LOP{
				ChainGRI:    m.ChainGRI,
				ChainName:   m.ChainName,
				MasterID:    m.MasterID,
				SecondaryID: m.SecondaryID,
				TDOA_US:     m.TDOA_US,
				Valid:       false,
				UpdatedAt:   m.UpdatedAt,
			})
			continue
		}

		// Look up transmitter positions from ChainDB.
		master, secondary, ok := findStationPositions(m.ChainGRI, m.MasterID, m.SecondaryID)
		if !ok {
			continue
		}

		lop := computeLOP(m, master, secondary, loranPropSpeedKmS, 200)
		lops = append(lops, lop)
	}

	e.mu.Lock()
	e.lops = lops
	e.mu.Unlock()
}

// LOPs returns a copy of the latest computed Lines of Position.
func (e *LOPEngine) LOPs() []LOP {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]LOP, len(e.lops))
	copy(out, e.lops)
	return out
}

// ---------------------------------------------------------------------------
// Position fix — Gauss-Newton least-squares
// ---------------------------------------------------------------------------

// PositionFix holds the result of a Gauss-Newton position fix.
type PositionFix struct {
	Lat        float64   `json:"lat"`
	Lon        float64   `json:"lon"`
	RMS_km     float64   `json:"rms_km"`     // RMS residual in km
	Iterations int       `json:"iterations"` // Gauss-Newton iterations used
	LOPCount   int       `json:"lop_count"`  // number of valid LOPs used
	Valid      bool      `json:"valid"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ComputeFix computes a position fix from a set of TDOA measurements using
// Gauss-Newton least-squares iteration.
//
// initialPos is the starting estimate (e.g. receiver position from /api/description,
// or the midpoint of the master–secondary baseline).
// propSpeedKmS is the propagation speed (loranPropSpeedKmS).
// maxIter is the maximum number of Gauss-Newton iterations (typically 20).
func ComputeFix(measurements []TDOAMeasurement, initialPos LatLon, propSpeedKmS float64, maxIter int) PositionFix {
	if maxIter <= 0 {
		maxIter = 20
	}

	// Collect valid measurements with known station positions.
	type obs struct {
		master    LatLon
		secondary LatLon
		rangeDiff float64 // c_prop × TDOA_us × 1e-6 (km)
	}
	var observations []obs
	for _, m := range measurements {
		if !m.Valid {
			continue
		}
		master, secondary, ok := findStationPositions(m.ChainGRI, m.MasterID, m.SecondaryID)
		if !ok {
			continue
		}
		observations = append(observations, obs{
			master:    master,
			secondary: secondary,
			rangeDiff: propSpeedKmS * m.TDOA_US * 1e-6,
		})
	}

	if len(observations) < 2 {
		return PositionFix{Valid: false, UpdatedAt: time.Now()}
	}

	// Gauss-Newton iteration.
	// State vector: [lat (deg), lon (deg)]
	lat := initialPos.Lat
	lon := initialPos.Lon

	var iter int
	for iter = 0; iter < maxIter; iter++ {
		p := LatLon{Lat: lat, Lon: lon}

		// Build Jacobian J (n×2) and residual vector r (n×1).
		n := len(observations)
		J := make([][]float64, n)
		res := make([]float64, n)

		for i, ob := range observations {
			dM := vincentyDistKm(p, ob.master)
			dS := vincentyDistKm(p, ob.secondary)
			predicted := dM - dS
			res[i] = ob.rangeDiff - predicted

			// Numerical Jacobian: partial derivatives w.r.t. lat and lon.
			// Step size: ~1 m in each direction.
			dLat := 1.0 / 111.32 // 1 m in degrees latitude
			dLon := 1.0 / (111.32 * math.Cos(lat*math.Pi/180))

			pLat := LatLon{Lat: lat + dLat, Lon: lon}
			pLon := LatLon{Lat: lat, Lon: lon + dLon}

			dMlat := vincentyDistKm(pLat, ob.master)
			dSlat := vincentyDistKm(pLat, ob.secondary)
			dMlon := vincentyDistKm(pLon, ob.master)
			dSlon := vincentyDistKm(pLon, ob.secondary)

			J[i] = []float64{
				(dMlat - dSlat - predicted) / dLat,
				(dMlon - dSlon - predicted) / dLon,
			}
		}

		// Solve normal equations: (JᵀJ) δ = Jᵀ r
		// 2×2 system — solve analytically.
		var JtJ [2][2]float64
		var Jtr [2]float64
		for i := 0; i < n; i++ {
			JtJ[0][0] += J[i][0] * J[i][0]
			JtJ[0][1] += J[i][0] * J[i][1]
			JtJ[1][0] += J[i][1] * J[i][0]
			JtJ[1][1] += J[i][1] * J[i][1]
			Jtr[0] += J[i][0] * res[i]
			Jtr[1] += J[i][1] * res[i]
		}

		det := JtJ[0][0]*JtJ[1][1] - JtJ[0][1]*JtJ[1][0]
		if math.Abs(det) < 1e-20 {
			break // singular — stop iterating
		}

		δLat := (JtJ[1][1]*Jtr[0] - JtJ[0][1]*Jtr[1]) / det
		δLon := (JtJ[0][0]*Jtr[1] - JtJ[1][0]*Jtr[0]) / det

		lat += δLat
		lon += δLon

		// Convergence check: step < 1 m.
		stepKm := math.Sqrt(δLat*δLat*111.32*111.32 + δLon*δLon*111.32*111.32)
		if stepKm < 0.001 {
			iter++
			break
		}
	}

	// Compute RMS residual.
	p := LatLon{Lat: lat, Lon: lon}
	var sumSq float64
	for _, ob := range observations {
		dM := vincentyDistKm(p, ob.master)
		dS := vincentyDistKm(p, ob.secondary)
		r := ob.rangeDiff - (dM - dS)
		sumSq += r * r
	}
	rms := math.Sqrt(sumSq / float64(len(observations)))

	return PositionFix{
		Lat:        lat,
		Lon:        lon,
		RMS_km:     rms,
		Iterations: iter,
		LOPCount:   len(observations),
		Valid:      true,
		UpdatedAt:  time.Now(),
	}
}

// ---------------------------------------------------------------------------
// Helper: look up transmitter positions from ChainDB
// ---------------------------------------------------------------------------

// findStationPositions looks up the master and secondary station positions
// for a given chain GRI and station IDs.
// Returns (master, secondary, true) on success, or (zero, zero, false) if
// either station has unknown coordinates (Lat==0 && Lon==0).
func findStationPositions(gri uint32, masterID, secondaryID string) (master, secondary LatLon, ok bool) {
	for _, chain := range ChainDB {
		if chain.GRI != gri {
			continue
		}
		var masterFound, secondaryFound bool
		for _, st := range chain.Stations {
			if st.ID == masterID {
				if st.Lat == 0 && st.Lon == 0 {
					return LatLon{}, LatLon{}, false
				}
				master = LatLon{Lat: st.Lat, Lon: st.Lon}
				masterFound = true
			}
			if st.ID == secondaryID {
				if st.Lat == 0 && st.Lon == 0 {
					return LatLon{}, LatLon{}, false
				}
				secondary = LatLon{Lat: st.Lat, Lon: st.Lon}
				secondaryFound = true
			}
		}
		if masterFound && secondaryFound {
			return master, secondary, true
		}
	}
	return LatLon{}, LatLon{}, false
}
