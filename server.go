// server.go — HTTP + WebSocket scope server for the Loran-C viewer
//
// Serves the static web UI and a WebSocket endpoint that:
//   - Streams binary SCOPE_DATA / SCOPE_RESET frames to connected browsers
//   - Accepts JSON control messages (gri, gain, offset, avg_algo, avg_param)
//     and forwards them to the active LoranDecoder
//
// Proxy-aware: reads X-Forwarded-Prefix header (set by the UberSDR addon proxy)
// and injects it as BasePath into the index.html template so that all asset
// URLs and the WebSocket URL are correctly prefixed when served behind
// /addon/loran/.

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"text/template"

	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// Wire protocol — binary frame sent to the browser
//
// Matches the format expected by the Loran_C.js frontend:
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
// active LoranDecoder instance.
type ScopeServer struct {
	staticDir string
	indexTmpl *template.Template

	clientsMu sync.RWMutex
	clients   map[*scopeClient]struct{}

	decoderMu sync.RWMutex
	decoder   *LoranDecoder

	mux *http.ServeMux
}

// NewScopeServer creates a new ScopeServer serving static files from staticDir.
// index.html is parsed as a Go template so that {{.BasePath}} is substituted
// with the X-Forwarded-Prefix header value at request time.
func NewScopeServer(staticDir string) *ScopeServer {
	tmpl, err := template.ParseFiles(staticDir + "/index.html")
	if err != nil {
		log.Fatalf("failed to parse index.html template: %v", err)
	}

	s := &ScopeServer{
		staticDir: staticDir,
		indexTmpl: tmpl,
		clients:   make(map[*scopeClient]struct{}),
	}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/ws", s.handleWS)
	s.mux.HandleFunc("/", s.handleHTTP)
	return s
}

// basePath extracts the X-Forwarded-Prefix header value, stripping any
// trailing slash.  Returns "" when accessed directly (not via proxy).
func basePath(r *http.Request) string {
	bp := r.Header.Get("X-Forwarded-Prefix")
	return strings.TrimRight(bp, "/")
}

// handleHTTP serves index.html as a template for /, and static files for
// everything else (loran_c.js, style.css, etc.).
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

// BroadcastScope sends a scope frame to all connected browser clients.
// Called from the decoder callback in main.go.
//
// Wire format (matches KiwiSDR ext_send_msg_data / Loran_C.js loran_c_recv):
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
func (s *ScopeServer) broadcastText(b []byte) {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	for c := range s.clients {
		select {
		case c.send <- append([]byte{0xFF}, b...): // 0xFF prefix = text frame marker
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
		// Send as text message directly (before the write pump starts).
		conn.WriteMessage(websocket.TextMessage, b) //nolint:errcheck
	}

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

	// Read pump — handles control messages from the browser.
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			close(c.send)
			return
		}
		s.handleControl(msg)
	}
}

// handleControl processes a JSON control message from the browser.
//
// Supported message types:
//
//	{ "type": "set_gri",       "ch": 0, "gri": 8000 }
//	{ "type": "set_gain",      "ch": 0, "gain": 0 }
//	{ "type": "set_offset",    "ch": 0, "offset": 42 }
//	{ "type": "set_avg_algo",  "ch": 0, "algo": 1 }
//	{ "type": "set_avg_param", "ch": 0, "param": 256.0 }
//	{ "type": "start" }
//
// ch may be 0 … nch-1.
func (s *ScopeServer) handleControl(msg []byte) {
	var m map[string]interface{}
	if err := json.Unmarshal(msg, &m); err != nil {
		log.Printf("control parse error: %v", err)
		return
	}

	s.decoderMu.RLock()
	d := s.decoder
	s.decoderMu.RUnlock()
	if d == nil {
		return
	}

	msgType, _ := m["type"].(string)
	ch := int(floatVal(m, "ch"))
	if ch < 0 || ch >= nch {
		ch = 0
	}

	switch msgType {
	case "set_gri":
		gri := uint32(floatVal(m, "gri"))
		if gri > 0 && gri <= maxGRI {
			d.SetGRI(ch, gri)
			log.Printf("set_gri ch%d gri=%d", ch, gri)
		}

	case "set_gain":
		gain := int(floatVal(m, "gain"))
		d.SetGain(ch, gain)
		log.Printf("set_gain ch%d gain=%d", ch, gain)

	case "set_offset":
		offset := int(floatVal(m, "offset"))
		d.SetOffset(ch, offset)
		log.Printf("set_offset ch%d offset=%d", ch, offset)

	case "set_avg_algo":
		algo := int(floatVal(m, "algo"))
		if algo >= avgCMA && algo <= avgIIR {
			d.SetAvgAlgo(ch, algo)
			log.Printf("set_avg_algo ch%d algo=%d", ch, algo)
		}

	case "set_avg_param":
		param := floatVal(m, "param")
		d.SetAvgParam(ch, param)
		log.Printf("set_avg_param ch%d param=%.2f", ch, param)

	case "start":
		d.Start()
		log.Printf("start")

	default:
		log.Printf("unknown control type: %s", msgType)
	}
}

func (s *ScopeServer) clientCount() int {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	return len(s.clients)
}

// floatVal safely extracts a float64 from a JSON map value.
func floatVal(m map[string]interface{}, key string) float64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case string:
		var f float64
		fmt.Sscanf(t, "%f", &f)
		return f
	}
	return 0
}
