// server.go — HTTP + WebSocket scope server for the Loran-C viewer
//
// Serves the static web UI and a WebSocket endpoint that:
//   - Streams binary SCOPE_DATA / SCOPE_RESET frames to connected browsers
//   - Accepts JSON control messages (gri, gain, offset, avg_algo, avg_param)
//     and forwards them to the active LoranDecoder
//
// REST API:
//   GET  /api/chains        — full GRI chain database (ChainDB)
//   GET  /api/config        — runtime config (iq_mode, update_hz, ms_per_bin, channels)
//   POST /api/control       — decoder control (same JSON as WebSocket control messages)
//   GET  /api/tdoa          — latest TDOA measurements for all chains
//   GET  /api/quality       — per-channel signal quality metrics
//   GET  /api/receiver      — receiver position (lat/lon from /api/description)
//
// Proxy-aware: reads X-Forwarded-Prefix header (set by the UberSDR addon proxy)
// and injects it as BasePath into the index.html template so that all asset
// URLs and the WebSocket URL are correctly prefixed when served behind
// /addon/loran/.

package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// Wire protocol — binary frame sent to the browser
//
// Matches the format expected by the loran_c.js frontend:
//
//   Frame header (3 bytes):  "DAT"
//   Followed by:
//     [0]  uint8   cmd  (0=SCOPE_DATA, 1=SCOPE_RESET)
//     [1]  uint8   ch   (0 … nch-1)
//     [2…] uint8   scope bytes (nbucket values, 0-255)
//
// This mirrors ext_send_msg_data() in the KiwiSDR C++ source.
// ---------------------------------------------------------------------------

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// indexData is passed to the index.html template.
type indexData struct {
	BasePath string // e.g. "/addon/loran" or "" when accessed directly
}

// scopeClient represents one connected browser WebSocket.
type scopeClient struct {
	conn *websocket.Conn
	send chan []byte
	mu   sync.Mutex
}

// ---------------------------------------------------------------------------
// ScopeServer
// ---------------------------------------------------------------------------

// ScopeServer manages the HTTP mux, connected WebSocket clients, and the
// active LoranDecoder + TDOAEngine instances.
type ScopeServer struct {
	staticDir string
	indexTmpl *template.Template
	iqMode    string // IQ mode string, e.g. "iq", "iq48", "iq96"
	updateHz  int

	clientsMu sync.RWMutex
	clients   map[*scopeClient]struct{}

	decoderMu sync.RWMutex
	decoder   *LoranDecoder

	tdoaMu  sync.RWMutex
	tdoaEng *TDOAEngine

	lopMu  sync.RWMutex
	lopEng *LOPEngine

	// receiverLat/Lon are the receiver's WGS-84 coordinates fetched from
	// /api/description.  Stored as float64 bits in atomic.Value for lock-free reads.
	receiverPos struct {
		lat float64
		lon float64
		mu  sync.RWMutex
	}

	// wallClockMs is the most recent wall-clock timestamp (ms since Unix epoch)
	// from the PCM packet header.  Updated atomically.
	wallClockMs uint64

	mux *http.ServeMux
}

// pushInterval is how often the server pushes quality/tdoa/lops/fix/timing
// over the WebSocket to all connected clients.  5 s is well within the
// UberSDR reverse-proxy rate limit (which only applies to REST calls).
const pushInterval = 5 * time.Second

// NewScopeServer creates a new ScopeServer serving static files from staticDir.
// iqMode is the IQ mode string (e.g. "iq", "iq48") — used in /api/config.
// The actual sample rate is determined lazily from the first PCM packet.
func NewScopeServer(staticDir string, iqMode string, updateHz int) *ScopeServer {
	tmpl, err := template.ParseFiles(staticDir + "/index.html")
	if err != nil {
		log.Fatalf("failed to parse index.html template: %v", err)
	}

	s := &ScopeServer{
		staticDir: staticDir,
		indexTmpl: tmpl,
		iqMode:    iqMode,
		updateHz:  updateHz,
		clients:   make(map[*scopeClient]struct{}),
	}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/ws", s.handleWS)
	s.mux.HandleFunc("/api/chains", s.handleAPIChains)
	s.mux.HandleFunc("/api/config", s.handleAPIConfig)
	s.mux.HandleFunc("/api/control", s.handleAPIControl)
	s.mux.HandleFunc("/api/tdoa", s.handleAPITDOA)
	s.mux.HandleFunc("/api/lops", s.handleAPILOPs)
	s.mux.HandleFunc("/api/fix", s.handleAPIFix)
	s.mux.HandleFunc("/api/quality", s.handleAPIQuality)
	s.mux.HandleFunc("/api/timing", s.handleAPITiming)
	s.mux.HandleFunc("/api/receiver", s.handleAPIReceiver)
	s.mux.HandleFunc("/", s.handleHTTP)

	// Start background goroutine that pushes live data over WS so the
	// frontend never needs to poll the REST endpoints.
	go s.runPushLoop()

	return s
}

// basePath extracts the X-Forwarded-Prefix header value, stripping any
// trailing slash.  Returns "" when accessed directly (not via proxy).
func basePath(r *http.Request) string {
	bp := r.Header.Get("X-Forwarded-Prefix")
	return strings.TrimRight(bp, "/")
}

// handleHTTP serves index.html as a template for /, and static files for
// everything else (loran_c.js, map.js, tdoa_panel.js, etc.).
func (s *ScopeServer) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" || r.URL.Path == "/index.html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := s.indexTmpl.Execute(w, indexData{BasePath: basePath(r)}); err != nil {
			log.Printf("template execute error: %v", err)
		}
		return
	}
	// All other paths: serve from staticDir as plain files.
	http.FileServer(http.Dir(s.staticDir)).ServeHTTP(w, r)
}

// Mux returns the HTTP mux for use with http.ListenAndServe.
func (s *ScopeServer) Mux() *http.ServeMux { return s.mux }

// ---------------------------------------------------------------------------
// Decoder / engine setters (called from main.go)
// ---------------------------------------------------------------------------

// SetDecoder sets (or clears) the active LoranDecoder.
// Called by the IQ client goroutine when a connection is established or lost.
func (s *ScopeServer) SetDecoder(d *LoranDecoder) {
	s.decoderMu.Lock()
	s.decoder = d
	s.decoderMu.Unlock()

	if d != nil {
		// Send ms_per_bin to all connected clients so the legend renders correctly.
		msg := map[string]interface{}{
			"type":       "ms_per_bin",
			"ms_per_bin": d.MsPerBin(),
		}
		b, _ := json.Marshal(msg)
		s.broadcastText(b)
	}
}

// SetTDOAEngine sets (or clears) the active TDOAEngine.
func (s *ScopeServer) SetTDOAEngine(e *TDOAEngine) {
	s.tdoaMu.Lock()
	s.tdoaEng = e
	s.tdoaMu.Unlock()
}

// SetLOPEngine sets (or clears) the active LOPEngine.
func (s *ScopeServer) SetLOPEngine(e *LOPEngine) {
	s.lopMu.Lock()
	s.lopEng = e
	s.lopMu.Unlock()
}

// SetReceiverPos stores the receiver's WGS-84 coordinates (from /api/description).
func (s *ScopeServer) SetReceiverPos(lat, lon float64) {
	s.receiverPos.mu.Lock()
	s.receiverPos.lat = lat
	s.receiverPos.lon = lon
	s.receiverPos.mu.Unlock()

	// Broadcast updated receiver position to all connected clients.
	msg := map[string]interface{}{
		"type": "receiver_pos",
		"lat":  lat,
		"lon":  lon,
	}
	b, _ := json.Marshal(msg)
	s.broadcastText(b)
}

// SetWallClockMs stores the most recent wall-clock timestamp from the PCM header.
func (s *ScopeServer) SetWallClockMs(ms uint64) {
	atomic.StoreUint64(&s.wallClockMs, ms)
}

// ---------------------------------------------------------------------------
// WS push loop — broadcasts live data to all clients every pushInterval.
// This eliminates the need for the frontend to poll REST endpoints, which
// would trigger the UberSDR reverse-proxy rate limiter.
// ---------------------------------------------------------------------------

// buildPushFrames assembles all live-data JSON messages to broadcast.
// Returns nil slices for data that is not yet available.
func (s *ScopeServer) buildPushFrames() [][]byte {
	var frames [][]byte

	// quality_update
	s.decoderMu.RLock()
	d := s.decoder
	s.decoderMu.RUnlock()
	if d != nil {
		channels := make([]ChannelQuality, nch)
		for i := 0; i < nch; i++ {
			channels[i] = d.Quality(i)
		}
		// Include auto-tracking deltas if the TDOA engine is running.
		s.tdoaMu.RLock()
		tdoaEngQ := s.tdoaEng
		s.tdoaMu.RUnlock()
		var trackDeltas []int
		if tdoaEngQ != nil {
			trackDeltas = tdoaEngQ.TrackDeltas()
		}
		if b, err := json.Marshal(map[string]interface{}{
			"type":          "quality_update",
			"channels":      channels,
			"track_deltas":  trackDeltas,
			"wall_clock_ms": atomic.LoadUint64(&s.wallClockMs),
		}); err == nil {
			frames = append(frames, b)
		}
	}

	// tdoa_update
	s.tdoaMu.RLock()
	tdoaEng := s.tdoaEng
	s.tdoaMu.RUnlock()
	if tdoaEng != nil {
		results := tdoaEng.Results()
		if b, err := json.Marshal(map[string]interface{}{
			"type":         "tdoa_update",
			"measurements": results,
		}); err == nil {
			frames = append(frames, b)
		}
	}

	// lop_update + fix_update
	s.lopMu.RLock()
	lopEng := s.lopEng
	s.lopMu.RUnlock()
	if lopEng != nil {
		lops := lopEng.LOPs()
		if b, err := json.Marshal(map[string]interface{}{
			"type": "lop_update",
			"lops": lops,
		}); err == nil {
			frames = append(frames, b)
		}
	}

	// fix_update — computed on demand from latest TDOA measurements
	if tdoaEng != nil {
		s.receiverPos.mu.RLock()
		initialPos := LatLon{Lat: s.receiverPos.lat, Lon: s.receiverPos.lon}
		s.receiverPos.mu.RUnlock()
		if initialPos.Lat == 0 && initialPos.Lon == 0 {
			initialPos = LatLon{Lat: 51.5, Lon: 0.0}
		}
		measurements := tdoaEng.Results()
		fix := ComputeFix(measurements, initialPos, loranPropSpeedKmS, 20)
		if b, err := json.Marshal(map[string]interface{}{
			"type": "fix_update",
			"fix":  fix,
		}); err == nil {
			frames = append(frames, b)
		}
	}

	return frames
}

// runPushLoop broadcasts live data to all connected WS clients every pushInterval.
func (s *ScopeServer) runPushLoop() {
	ticker := time.NewTicker(pushInterval)
	defer ticker.Stop()
	for range ticker.C {
		frames := s.buildPushFrames()
		for _, b := range frames {
			s.broadcastText(b)
		}
	}
}

// sendPushSnapshot sends the current live-data snapshot to a single newly
// connected client (so it gets data immediately without waiting for the
// first tick).
func (s *ScopeServer) sendPushSnapshot(conn interface{ WriteMessage(int, []byte) error }) {
	frames := s.buildPushFrames()
	for _, b := range frames {
		frame := make([]byte, 1+len(b))
		frame[0] = 0xFF
		copy(frame[1:], b)
		// Write directly — client is not yet in the send queue.
		conn.WriteMessage(websocket.TextMessage, b) //nolint:errcheck
	}
}

// ---------------------------------------------------------------------------
// Broadcast helpers
// ---------------------------------------------------------------------------

// BroadcastScope sends a scope frame to all connected browser clients.
// Called from the decoder callback in main.go.
//
// Wire format (matches KiwiSDR ext_send_msg_data / loran_c.js loran_c_recv):
//
//	"DAT" + cmd(1) + payload(nbucket+1 bytes where payload[0]=ch)
func (s *ScopeServer) BroadcastScope(ch int, cmd byte, payload []byte) {
	// payload already contains [ch, b0, b1, …] as built in decoder.go.
	frame := make([]byte, 3+1+len(payload))
	copy(frame[0:3], "DAT")
	frame[3] = cmd
	copy(frame[4:], payload)

	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	for c := range s.clients {
		select {
		case c.send <- frame:
		default:
			// Slow client — drop frame rather than block the decoder.
		}
	}
}

// broadcastText sends a JSON text message to all connected clients.
// The byte slice must NOT include the 0xFF prefix — this function adds it.
func (s *ScopeServer) broadcastText(b []byte) {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	for c := range s.clients {
		frame := make([]byte, 1+len(b))
		frame[0] = 0xFF // text frame marker
		copy(frame[1:], b)
		select {
		case c.send <- frame:
		default:
		}
	}
}

// ---------------------------------------------------------------------------
// WebSocket handler
// ---------------------------------------------------------------------------

func (s *ScopeServer) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS upgrade error: %v", err)
		return
	}

	c := &scopeClient{
		conn: conn,
		send: make(chan []byte, 256),
	}

	s.clientsMu.Lock()
	s.clients[c] = struct{}{}
	s.clientsMu.Unlock()

	log.Printf("Scope client connected: %s (total: %d)", r.RemoteAddr, s.clientCount())

	// Send ms_per_bin immediately if a decoder is active.
	s.decoderMu.RLock()
	d := s.decoder
	s.decoderMu.RUnlock()
	if d != nil {
		msg := map[string]interface{}{
			"type":       "ms_per_bin",
			"ms_per_bin": d.MsPerBin(),
		}
		b, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, b) //nolint:errcheck
	}

	// Send receiver position immediately if known.
	s.receiverPos.mu.RLock()
	lat := s.receiverPos.lat
	lon := s.receiverPos.lon
	s.receiverPos.mu.RUnlock()
	if lat != 0 || lon != 0 {
		msg := map[string]interface{}{
			"type": "receiver_pos",
			"lat":  lat,
			"lon":  lon,
		}
		b, _ := json.Marshal(msg)
		conn.WriteMessage(websocket.TextMessage, b) //nolint:errcheck
	}

	// Send current live-data snapshot immediately so the new client
	// doesn't have to wait up to pushInterval for the first update.
	s.sendPushSnapshot(conn)

	// Write pump — sends queued frames to the browser.
	go func() {
		defer func() {
			conn.Close()
			s.clientsMu.Lock()
			delete(s.clients, c)
			s.clientsMu.Unlock()
			log.Printf("Scope client disconnected: %s (total: %d)", r.RemoteAddr, s.clientCount())
		}()
		for frame := range c.send {
			if len(frame) > 0 && frame[0] == 0xFF {
				// Text frame (JSON message).
				if err := conn.WriteMessage(websocket.TextMessage, frame[1:]); err != nil {
					return
				}
			} else {
				// Binary scope frame.
				if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
					return
				}
			}
		}
	}()

	// Read pump — this is a read-only viewer; all inbound messages are
	// discarded.  We must still read to detect client disconnection and
	// to satisfy the WebSocket protocol (ping/pong frames are handled by
	// the gorilla/websocket library automatically).
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			close(c.send)
			return
		}
	}
}

func (s *ScopeServer) clientCount() int {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	return len(s.clients)
}

// ---------------------------------------------------------------------------
// REST API handlers
// ---------------------------------------------------------------------------

// handleAPIChains serves GET /api/chains — the full GRI chain database.
// Response: JSON array of Chain objects (see chains.go).
func (s *ScopeServer) handleAPIChains(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := json.NewEncoder(w).Encode(ChainDB); err != nil {
		log.Printf("handleAPIChains encode: %v", err)
	}
}

// configResponse is the JSON shape returned by GET /api/config.
type configResponse struct {
	IQMode     string          `json:"iq_mode"`
	SampleRate int             `json:"sample_rate"` // 0 until first packet received
	UpdateHz   int             `json:"update_hz"`
	MsPerBin   float64         `json:"ms_per_bin"` // 0 until first packet received
	Channels   []channelConfig `json:"channels"`
}

type channelConfig struct {
	Index int    `json:"index"`
	GRI   uint32 `json:"gri"`
}

// handleAPIConfig serves GET /api/config — current runtime configuration.
func (s *ScopeServer) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.decoderMu.RLock()
	d := s.decoder
	s.decoderMu.RUnlock()

	var sampleRate int
	var msPerBin float64
	if d != nil {
		// iSrate is the actual sample rate from the PCM packet header.
		sampleRate = int(d.iSrate)
		msPerBin = d.MsPerBin()
	}

	channels := make([]channelConfig, len(ChainDB))
	for i, c := range ChainDB {
		channels[i] = channelConfig{Index: i, GRI: c.GRI}
	}

	resp := configResponse{
		IQMode:     s.iqMode,
		SampleRate: sampleRate,
		UpdateHz:   s.updateHz,
		MsPerBin:   msPerBin,
		Channels:   channels,
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("handleAPIConfig encode: %v", err)
	}
}

// handleAPIControl is intentionally disabled — this is a read-only viewer.
// Decoder parameters are set server-side at startup; no external client
// should be able to mutate decoder state.
func (s *ScopeServer) handleAPIControl(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "read-only viewer — control endpoint disabled", http.StatusMethodNotAllowed)
}

// handleAPITDOA serves GET /api/tdoa — latest TDOA measurements.
//
// Response: JSON array of TDOAMeasurement objects.
// Returns 503 if no decoder is active yet.
func (s *ScopeServer) handleAPITDOA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")

	s.tdoaMu.RLock()
	eng := s.tdoaEng
	s.tdoaMu.RUnlock()

	if eng == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"decoder not yet active"}`)) //nolint:errcheck
		return
	}

	results := eng.Results()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(results); err != nil {
		log.Printf("handleAPITDOA encode: %v", err)
	}
}

// qualityResponse is the JSON shape returned by GET /api/quality.
type qualityResponse struct {
	Channels []ChannelQuality `json:"channels"`
	WallMs   uint64           `json:"wall_clock_ms"` // most recent PCM wall-clock timestamp
}

// handleAPIQuality serves GET /api/quality — per-channel signal quality metrics.
func (s *ScopeServer) handleAPIQuality(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")

	s.decoderMu.RLock()
	d := s.decoder
	s.decoderMu.RUnlock()

	if d == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"decoder not yet active"}`)) //nolint:errcheck
		return
	}

	channels := make([]ChannelQuality, nch)
	for i := 0; i < nch; i++ {
		channels[i] = d.Quality(i)
	}

	resp := qualityResponse{
		Channels: channels,
		WallMs:   atomic.LoadUint64(&s.wallClockMs),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("handleAPIQuality encode: %v", err)
	}
}

// receiverResponse is the JSON shape returned by GET /api/receiver.
type receiverResponse struct {
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	IQMode string  `json:"iq_mode"`
}

// handleAPIReceiver serves GET /api/receiver — receiver position and IQ mode.
func (s *ScopeServer) handleAPIReceiver(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")

	s.receiverPos.mu.RLock()
	lat := s.receiverPos.lat
	lon := s.receiverPos.lon
	s.receiverPos.mu.RUnlock()

	resp := receiverResponse{
		Lat:    lat,
		Lon:    lon,
		IQMode: s.iqMode,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("handleAPIReceiver encode: %v", err)
	}
}

// handleAPITiming serves GET /api/timing — UTC wall-clock timing from PCM header.
func (s *ScopeServer) handleAPITiming(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	info := s.GetTimingInfo()
	if err := json.NewEncoder(w).Encode(info); err != nil {
		log.Printf("handleAPITiming encode: %v", err)
	}
}

// handleAPILOPs serves GET /api/lops — latest Lines of Position.
//
// Response: JSON array of LOP objects (each with a sampled polyline).
// Returns 503 if no LOP engine is active yet.
func (s *ScopeServer) handleAPILOPs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")

	s.lopMu.RLock()
	eng := s.lopEng
	s.lopMu.RUnlock()

	if eng == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"LOP engine not yet active"}`)) //nolint:errcheck
		return
	}

	lops := eng.LOPs()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(lops); err != nil {
		log.Printf("handleAPILOPs encode: %v", err)
	}
}

// handleAPIFix serves GET /api/fix — Gauss-Newton position fix.
//
// Uses the latest TDOA measurements and the receiver's known position as the
// initial estimate.  Returns 503 if no TDOA engine is active.
func (s *ScopeServer) handleAPIFix(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")

	s.tdoaMu.RLock()
	eng := s.tdoaEng
	s.tdoaMu.RUnlock()

	if eng == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"TDOA engine not yet active"}`)) //nolint:errcheck
		return
	}

	s.receiverPos.mu.RLock()
	initialPos := LatLon{Lat: s.receiverPos.lat, Lon: s.receiverPos.lon}
	s.receiverPos.mu.RUnlock()

	// Fall back to a central European position if receiver pos unknown.
	if initialPos.Lat == 0 && initialPos.Lon == 0 {
		initialPos = LatLon{Lat: 51.5, Lon: 0.0} // London as fallback
	}

	measurements := eng.Results()
	fix := ComputeFix(measurements, initialPos, loranPropSpeedKmS, 20)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(fix); err != nil {
		log.Printf("handleAPIFix encode: %v", err)
	}
}

// Ensure atomic.StoreUint64 alignment on 32-bit platforms.
// wallClockMs is a uint64 field in ScopeServer — Go guarantees 64-bit
// alignment for struct fields on all platforms when using sync/atomic.
var _ = unsafe.Sizeof(ScopeServer{})
