// ubersdr_loran — Loran-C pulse-envelope viewer for UberSDR
//
// Connects to an UberSDR instance, requests an IQ stream centred on 100 kHz,
// decodes the Loran-C pulse envelope for all 14 simultaneous GRI chains,
// and serves a browser-based scope display + TDOA/position fix on a local HTTP port.
//
// Usage:
//
//	ubersdr_loran [flags]
//	  -url      string   UberSDR base URL, e.g. http://host:8080  (required)
//	  -pass     string   Bypass password (optional)
//	  -web-port int      Port for the scope web UI (default: 6088)
//	  -web-static string Path to static web files (default: ./static)
//	  -update-hz int     Scope update rate in Hz (default: 10)
//	  -no-reconnect      Disable auto-reconnect on disconnect
//
// The IQ mode ("iq" by default, giving ±5 kHz / 10 kHz sample rate) can be
// overridden with -mode.  Wide IQ modes require a bypass password:
//
//	"iq"    → ±5 kHz   → 10,000 Hz sample rate → 100 µs/bin
//	"iq48"  → ±24 kHz  → 48,000 Hz sample rate → ~20.8 µs/bin
//	"iq96"  → ±48 kHz  → 96,000 Hz sample rate → ~10.4 µs/bin
//	"iq192" → ±96 kHz  → 192,000 Hz sample rate → ~5.2 µs/bin
//	"iq384" → ±192 kHz → 384,000 Hz sample rate → ~2.6 µs/bin
//
// The actual sample rate is read from the PCM packet header on the first
// packet — no hardcoded assumptions are made.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/madpsy/ubersdr_loran/internal/pcmv4"
)

// loranFrequencyHz is the Loran-C carrier frequency.
const loranFrequencyHz = 100000

// defaultWebPort is the default port for the scope web UI.
const defaultWebPort = 6088

const rcvBufSize = 4 * 1024 * 1024 // 4 MiB SO_RCVBUF

// wsDialer sets SO_RCVBUF on the underlying TCP socket before the WebSocket handshake.
var wsDialer = &websocket.Dialer{
	HandshakeTimeout: 10 * time.Second,
	NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		nd := &net.Dialer{}
		conn, err := nd.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			raw, err := tc.SyscallConn()
			if err == nil {
				_ = raw.Control(func(fd uintptr) {
					_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvBufSize)
				})
			}
		}
		return conn, nil
	},
}

// ---------------------------------------------------------------------------
// Protocol types (mirrors the UberSDR server protocol)
// ---------------------------------------------------------------------------

type connectionCheckRequest struct {
	UserSessionID string `json:"user_session_id"`
	Password      string `json:"password,omitempty"`
}

type connectionCheckResponse struct {
	Allowed        bool     `json:"allowed"`
	Reason         string   `json:"reason,omitempty"`
	ClientIP       string   `json:"client_ip,omitempty"`
	Bypassed       bool     `json:"bypassed"`
	AllowedIQModes []string `json:"allowed_iq_modes,omitempty"`
	MaxSessionTime int      `json:"max_session_time"`
}

type wsMessage struct {
	Type      string `json:"type"`
	Error     string `json:"error,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Frequency int    `json:"frequency,omitempty"`
	Mode      string `json:"mode,omitempty"`
}

// ---------------------------------------------------------------------------
// UberSDR /api/description response (subset of fields we care about)
// ---------------------------------------------------------------------------

// receiverDescription holds the fields we need from GET /api/description.
// The full response contains many more fields; we only decode what we use.
type receiverDescription struct {
	Receiver struct {
		Name     string `json:"name"`
		Callsign string `json:"callsign"`
		Location string `json:"location"`
		GPS      struct {
			Lat        float64 `json:"lat"`
			Lon        float64 `json:"lon"`
			Maidenhead string  `json:"maidenhead"`
			GPSEnabled bool    `json:"gps_enabled"`
		} `json:"gps"`
	} `json:"receiver"`
}

// fetchDescription calls GET /api/description on the UberSDR server and
// returns the parsed response.  Errors are non-fatal — callers fall back
// gracefully (e.g. receiver position stays at 0,0).
func fetchDescription(httpBase string) (*receiverDescription, error) {
	endpoint := strings.TrimRight(httpBase, "/") + "/api/description"
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ubersdr_loran/1.0")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /api/description returned HTTP %d", resp.StatusCode)
	}

	var desc receiverDescription
	if err := json.NewDecoder(resp.Body).Decode(&desc); err != nil {
		return nil, fmt.Errorf("decode /api/description: %w", err)
	}
	return &desc, nil
}

// ---------------------------------------------------------------------------
// PCM binary packet decoder (audio protocol version 4)
// ---------------------------------------------------------------------------
//
// This tool asks the UberSDR server for protocol version 4, which replaced the
// zstd-wrapped versions 1-3 shape this file used to parse. Two things changed
// on the wire and neither is visible past this file:
//
//   - the payload is no longer zstd around big-endian int16 samples but a
//     predictive lossless code (internal/pcmv4/pcm_predictive.go) that emits
//     int16 directly, so the per-packet byte swap is gone;
//   - the fixed 29- or 37-byte header became a variable one carrying only what
//     changed since the last packet, so there is no "full" and "minimal" packet
//     distinction and no RTP anchor to extrapolate the clock from. Sample rate,
//     channel count and timestamp are carried forward by the header decoder and
//     are populated on every packet, so the sample rate the Loran decoder is
//     built from and the ±5 kHz interleaved I/Q ordering it consumes are as
//     they were.
//
// zstd never compressed this material -- it is an LZ77 matcher over bytes, and
// a band-limited RF signal has no repeated byte strings, so every IQ mode
// measured at 0.99x: the wrapped stream was larger than the samples it carried.
//
// The decoder is stateful and backward adaptive: it derives its predictor from
// the samples already decoded and never receives a coefficient, so it belongs
// to exactly one WebSocket connection. runOnce() builds a fresh one per
// connection and drops it when the socket closes; carrying one across a
// reconnect would decode the new stream against the old stream's adaptation and
// produce plausible noise rather than an error.

type pcmDecoder struct {
	v4 *pcmv4.PCMv4StreamDecoder
}

// newPCMDecoder returns a decoder for one connection. It holds no state until
// the first packet carrying metadata arrives, which the server sends at the
// head of every stream and every five seconds after.
func newPCMDecoder() *pcmDecoder {
	return &pcmDecoder{v4: pcmv4.NewPCMv4StreamDecoder()}
}

// decode parses one binary WebSocket message.
//
// Returns the samples -- interleaved I/Q when the header reports two channels,
// which is every mode this tool uses -- the sample rate, the channel count, and
// the packet's wall-clock timestamp in milliseconds.
//
// The timestamp is the same quantity versions 1-3 carried in bytes [12:20] of
// their full header: the server writes GPS-synchronised nanoseconds and those
// headers carried it divided by a million, so timing.go's reading of it is
// unchanged. It is zero when radiod reported no time.
func (d *pcmDecoder) decode(data []byte) ([]int16, int, int, uint64, error) {
	// A server older than 0.1.63 clamps a version request to 1-3 and answers
	// with version 1 without saying so. Naming that is what turns a dead stream
	// into a message the operator can act on.
	if pcmv4.IsZstdFrame(data) {
		return nil, 0, 0, 0, fmt.Errorf("server sent a zstd (protocol version 1) frame; it is too old for audio protocol version %d", pcmv4.ProtocolVersion)
	}
	if !pcmv4.PCMv4IsHeader(data) {
		return nil, 0, 0, 0, fmt.Errorf("not a version %d packet (%d bytes)", pcmv4.ProtocolVersion, len(data))
	}

	h, samples, err := d.v4.DecodePacket(data)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	return samples, h.SampleRate, h.Channels, h.TimestampNanos / 1e6, nil
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

type client struct {
	baseURL       string
	password      string
	iqMode        string // IQ mode: "iq", "iq48", "iq96", "iq192", "iq384"
	sessionID     string
	autoReconnect bool
	running       bool
	server        *ScopeServer
	updateHz      int
	avgAlgo       int     // averaging algorithm: 0=CMA, 1=EMA, 2=IIR
	avgParam      float64 // averaging parameter (meaning depends on algorithm)
}

func (c *client) httpBase() string {
	u, _ := url.Parse(c.baseURL)
	scheme := u.Scheme
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, u.Host)
}

func (c *client) wsURL() string {
	u, _ := url.Parse(c.baseURL)
	wsScheme := "ws"
	if u.Scheme == "https" || u.Scheme == "wss" {
		wsScheme = "wss"
	}

	path := strings.TrimRight(u.Path, "/")
	if path == "" {
		path = "/ws"
	}

	q := url.Values{}
	q.Set("frequency", fmt.Sprintf("%d", loranFrequencyHz))
	q.Set("mode", c.iqMode)
	q.Set("format", "pcm-zstd")
	// Audio protocol version 4: predictive lossless payload and a
	// variable-length header, decoded above. The format name is unchanged --
	// only the version moved. Sending no version at all, as this tool used to,
	// gets version 1: the server's floor, not its current format.
	q.Set("version", fmt.Sprintf("%d", pcmv4.ProtocolVersion))
	q.Set("user_session_id", c.sessionID)
	if c.password != "" {
		q.Set("password", c.password)
	}

	return fmt.Sprintf("%s://%s%s?%s", wsScheme, u.Host, path, q.Encode())
}

func (c *client) checkConnection() (bool, error) {
	endpoint := c.httpBase() + "/connection"

	body, _ := json.Marshal(connectionCheckRequest{
		UserSessionID: c.sessionID,
		Password:      c.password,
	})

	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ubersdr_loran/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection check failed (%v), attempting anyway\n", err)
		return true, nil
	}
	defer resp.Body.Close()

	var cr connectionCheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return false, fmt.Errorf("decode /connection response: %w", err)
	}

	if !cr.Allowed {
		return false, fmt.Errorf("server rejected connection: %s", cr.Reason)
	}

	fmt.Fprintf(os.Stderr, "connection allowed (IP: %s, bypassed: %v, max session: %ds)\n",
		cr.ClientIP, cr.Bypassed, cr.MaxSessionTime)
	return true, nil
}

func (c *client) runOnce() (reconnect bool) {
	c.sessionID = uuid.New().String()

	// Fetch /api/description to get receiver lat/lon and update the server.
	if desc, err := fetchDescription(c.httpBase()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not fetch /api/description: %v\n", err)
	} else {
		lat := desc.Receiver.GPS.Lat
		lon := desc.Receiver.GPS.Lon
		c.server.SetReceiverPos(lat, lon)
		fmt.Fprintf(os.Stderr, "receiver position: lat=%.6f lon=%.6f (%s) [%s]\n",
			lat, lon, desc.Receiver.GPS.Maidenhead, desc.Receiver.Name)
	}

	allowed, err := c.checkConnection()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return c.autoReconnect
	}
	if !allowed {
		return false
	}

	wsAddr := c.wsURL()
	fmt.Fprintf(os.Stderr, "connecting to %s\n", wsAddr)

	hdr := http.Header{}
	hdr.Set("User-Agent", "ubersdr_loran/1.0")
	conn, _, err := wsDialer.Dial(wsAddr, hdr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "websocket dial: %v\n", err)
		return c.autoReconnect
	}
	defer conn.Close()

	fmt.Fprintf(os.Stderr, "connected — freq=%d Hz, mode=%s (sample rate determined from first packet)\n",
		loranFrequencyHz, c.iqMode)

	// One decoder per connection. It is backward adaptive and stateful, so it
	// must not outlive this socket: see the decoder section above.
	dec := newPCMDecoder()

	// The Loran decoder is created lazily on the first full PCM packet so
	// that we use the actual sample rate from the packet header rather than
	// any hardcoded constant.
	var loranDec *LoranDecoder
	var tdoaEng *TDOAEngine

	// Keepalive goroutine.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := conn.WriteJSON(map[string]string{"type": "ping"}); err != nil {
					fmt.Fprintf(os.Stderr, "keepalive error: %v\n", err)
					return
				}
			}
		}
	}()

	for c.running {
		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				fmt.Fprintf(os.Stderr, "server closed connection\n")
			} else {
				fmt.Fprintf(os.Stderr, "read error: %v\n", err)
			}
			return c.autoReconnect
		}

		switch msgType {
		case websocket.BinaryMessage:
			samples, rate, ch, wallMs, err := dec.decode(msg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "decode error: %v\n", err)
				continue
			}
			if len(samples) == 0 {
				continue
			}

			// Lazy initialisation: create the Loran decoder on the first packet
			// using the actual sample rate from the PCM header.
			if loranDec == nil {
				fmt.Fprintf(os.Stderr, "receiving IQ: %d Hz sample rate, %d channel(s), mode=%s\n",
					rate, ch, c.iqMode)
				loranDec = NewLoranDecoder(float64(rate), c.updateHz)
				// Set GRIs for all chains from ChainDB before starting.
				// This is done server-side so no browser client needs to send
				// control messages (which would be a security risk on a public server).
				for chIdx, chain := range ChainDB {
					gri := chain.GRI
					// GRI 5991 (US west coast eLoran test) shares the 5990 transmitter
					if gri == 5991 {
						gri = 5990
					}
					loranDec.SetGRI(chIdx, gri)
					loranDec.SetAvgAlgo(chIdx, c.avgAlgo)
					loranDec.SetAvgParam(chIdx, c.avgParam)
				}
				loranDec.Start()
				tdoaEng = NewTDOAEngine(loranDec)
				lopEng := NewLOPEngine(tdoaEng)
				c.server.SetDecoder(loranDec)
				c.server.SetTDOAEngine(tdoaEng)
				c.server.SetLOPEngine(lopEng)
			}

			// Pass wall-clock timestamp to the timing engine (if available).
			if wallMs > 0 {
				c.server.SetWallClockMs(wallMs)
			}

			// The version 4 codec already produces int16 in the interleaved
			// I/Q order the Loran decoder expects, so the bytes-to-samples
			// conversion versions 1-3 needed is gone.
			//
			// Feed samples to the Loran-C decoder; broadcast any scope frames.
			// TDOA/LOP updates are driven by the push loop in server.go (every 5 s),
			// not here — calling them per-channel-frame (140×/s) was the CPU hot path.
			loranDec.ProcessSamples(samples, func(chIdx int, cmd byte, payload []byte) {
				c.server.BroadcastScope(chIdx, cmd, payload)
			})

		case websocket.TextMessage:
			var m wsMessage
			if err := json.Unmarshal(msg, &m); err != nil {
				fmt.Fprintf(os.Stderr, "json parse: %v\n", err)
				continue
			}
			switch m.Type {
			case "error":
				fmt.Fprintf(os.Stderr, "server error: %s\n", m.Error)
				return c.autoReconnect
			case "status":
				fmt.Fprintf(os.Stderr, "status: session=%s freq=%d mode=%s\n",
					m.SessionID, m.Frequency, m.Mode)
			case "pong":
				// keepalive ack — ignore
			}
		}
	}

	if loranDec != nil {
		c.server.SetDecoder(nil)
		c.server.SetTDOAEngine(nil)
		c.server.SetLOPEngine(nil)
	}
	return false
}

func (c *client) run() int {
	retries := 0
	maxBackoff := 60 * time.Second

	for {
		reconnect := c.runOnce()
		if !reconnect || !c.running {
			return 0
		}

		retries++
		shift := retries
		if shift > 6 { // cap at 2^6=64s to avoid int64 overflow on 1<<uint(shift)
			shift = 6
		}
		backoff := time.Duration(1<<uint(shift)) * time.Second
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		if backoff < 5*time.Second { // safety net: never reconnect faster than 5s
			backoff = 5 * time.Second
		}
		fmt.Fprintf(os.Stderr, "reconnecting in %.0fs (attempt %d)…\n", backoff.Seconds(), retries)

		select {
		case <-time.After(backoff):
		case <-func() <-chan struct{} {
			ch := make(chan struct{})
			go func() {
				for c.running {
					time.Sleep(100 * time.Millisecond)
				}
				close(ch)
			}()
			return ch
		}():
			return 0
		}
	}
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	var (
		rawURL    = flag.String("url", "", "UberSDR base URL, e.g. http://host:8080 (required)")
		pass      = flag.String("pass", "", "Bypass password (optional)")
		iqMode    = flag.String("mode", "iq", "IQ mode: iq (10kHz), iq48, iq96, iq192, iq384 (wide modes require bypass)")
		webPort   = flag.Int("web-port", defaultWebPort, "Port for the Loran-C scope web UI")
		webStatic = flag.String("web-static", "./static", "Path to static web files directory")
		updateHz  = flag.Int("update-hz", 10, "Scope update rate in Hz (1=KiwiSDR-compatible, 10=default, max ~25)")
		noReconn  = flag.Bool("no-reconnect", false, "Disable auto-reconnect on disconnect")
		avgAlgo   = flag.Int("avg-algo", avgEMA, "Averaging algorithm: 0=CMA, 1=EMA (default), 2=IIR")
		avgParam  = flag.Float64("avg-param", 256, "Averaging parameter (EMA: decay 1-512, CMA: periods 1-32, IIR: exp 0.0-1.0)")
	)
	flag.Parse()

	if *rawURL == "" {
		fmt.Fprintf(os.Stderr, "Usage: ubersdr_loran -url <http://host:port> [-pass <password>] [-mode <iq|iq48|iq96|iq192|iq384>] [-web-port <port>] [-web-static <path>] [-update-hz <hz>] [-no-reconnect]\n\n")
		fmt.Fprintf(os.Stderr, "  Connects to UberSDR at 100 kHz (Loran-C carrier) using the specified IQ mode\n")
		fmt.Fprintf(os.Stderr, "  and serves a Loran-C pulse-envelope scope + TDOA/position fix at http://localhost:%d/\n", defaultWebPort)
		os.Exit(1)
	}

	// Validate IQ mode.
	validModes := map[string]bool{
		"iq": true, "iq48": true, "iq96": true, "iq192": true, "iq384": true,
	}
	if !validModes[*iqMode] {
		fmt.Fprintf(os.Stderr, "error: invalid mode %q — must be one of: iq, iq48, iq96, iq192, iq384\n", *iqMode)
		os.Exit(1)
	}

	// Start the scope HTTP/WebSocket server.
	// Sample rate is 0 here — it will be updated from the first PCM packet.
	server := NewScopeServer(*webStatic, *iqMode, *updateHz)
	go func() {
		addr := fmt.Sprintf(":%d", *webPort)
		fmt.Fprintf(os.Stderr, "scope server listening on http://localhost%s/\n", addr)
		if err := http.ListenAndServe(addr, server.Mux()); err != nil {
			fmt.Fprintf(os.Stderr, "scope server error: %v\n", err)
			os.Exit(1)
		}
	}()

	c := &client{
		baseURL:       *rawURL,
		password:      *pass,
		iqMode:        *iqMode,
		sessionID:     uuid.New().String(),
		autoReconnect: !*noReconn,
		running:       true,
		server:        server,
		updateHz:      *updateHz,
		avgAlgo:       *avgAlgo,
		avgParam:      *avgParam,
	}

	// Handle SIGINT / SIGTERM gracefully.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Fprintf(os.Stderr, "\nshutting down\n")
		c.running = false
	}()

	os.Exit(c.run())
}
