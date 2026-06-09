// ubersdr_loran — Loran-C pulse-envelope viewer for UberSDR
//
// Connects to an UberSDR instance, requests an IQ stream centred on 100 kHz,
// decodes the Loran-C pulse envelope for up to two simultaneous GRI chains,
// and serves a browser-based scope display on a local HTTP port.
//
// Usage:
//
//	ubersdr_loran [flags]
//	  -url      string   UberSDR base URL, e.g. http://host:8080  (required)
//	  -pass     string   Bypass password (optional)
//	  -web-port int      Port for the scope web UI (default: 8095)
//	  -web-static string Path to static web files (default: ./static)
//	  -no-reconnect      Disable auto-reconnect on disconnect

package main

import (
	"bytes"
	"context"
	"encoding/binary"
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
	"github.com/klauspost/compress/zstd"
)

// loranFrequencyHz is the Loran-C carrier frequency.
const loranFrequencyHz = 100000

// iqMode is the UberSDR IQ mode used — "iq" gives 10 kHz bandwidth at 10,000 Hz sample rate,
// which is sufficient for Loran-C pulse envelope detection.
const iqMode = "iq"

// iqSampleRate is the sample rate delivered by the "iq" mode.
const iqSampleRate = 10000

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
// PCM binary packet decoder (mirrors ubersdr_hfdl/main.go)
// ---------------------------------------------------------------------------
//
// The server sends packets in the UberSDR hybrid binary format.
// Two packet types:
//
//   Full header  (magic 0x5043 "PC", 29 bytes):
//     [0:2]  uint16  magic
//     [2]    uint8   version
//     [3]    uint8   format (0=PCM, 2=PCM-zstd)
//     [4:12] uint64  RTP timestamp (LE)
//     [12:20]uint64  wall-clock ms (LE)
//     [20:24]uint32  sample rate (LE)
//     [24]   uint8   channels
//     [25:29]uint32  reserved
//     [29:]  []byte  PCM samples (big-endian int16)
//
//   Version 2 full header (37 bytes) adds signal quality fields:
//     [25:29]float32 baseband power dBFS
//     [29:33]float32 noise density dBFS
//     [33:37]uint32  reserved
//     [37:]  []byte  PCM samples (big-endian int16)
//
//   Minimal header (magic 0x504D "PM", 13 bytes):
//     [0:2]  uint16  magic
//     [2]    uint8   version
//     [3:11] uint64  RTP timestamp (LE)
//     [11:13]uint16  reserved
//     [13:]  []byte  PCM samples (big-endian int16)

const (
	magicFull    = 0x5043 // "PC"
	magicMinimal = 0x504D // "PM"
)

type pcmDecoder struct {
	zd           *zstd.Decoder
	lastRate     int
	lastChannels int
}

func newPCMDecoder() (*pcmDecoder, error) {
	zd, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("zstd init: %w", err)
	}
	return &pcmDecoder{zd: zd}, nil
}

// decode decompresses (if needed) and parses a binary PCM packet.
// Returns big-endian int16 samples converted to little-endian, sample rate, channel count.
func (d *pcmDecoder) decode(data []byte, isZstd bool) ([]byte, int, int, error) {
	if isZstd {
		var err error
		data, err = d.zd.DecodeAll(data, nil)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("zstd decompress: %w", err)
		}
	}

	if len(data) < 4 {
		return nil, 0, 0, fmt.Errorf("packet too short (%d bytes)", len(data))
	}

	magic := binary.LittleEndian.Uint16(data[0:2])

	var rate, ch int
	var raw []byte

	switch magic {
	case magicFull:
		version := data[2]
		var headerLen int
		switch version {
		case 2:
			headerLen = 37
		default: // version 1
			headerLen = 29
		}
		if len(data) < headerLen {
			return nil, 0, 0, fmt.Errorf("full-header packet too short (%d < %d)", len(data), headerLen)
		}
		rate = int(binary.LittleEndian.Uint32(data[20:24]))
		ch = int(data[24])
		raw = data[headerLen:]
		d.lastRate = rate
		d.lastChannels = ch

	case magicMinimal:
		if len(data) < 13 {
			return nil, 0, 0, fmt.Errorf("minimal-header packet too short (%d bytes)", len(data))
		}
		raw = data[13:]
		rate = d.lastRate
		ch = d.lastChannels
		if rate == 0 || ch == 0 {
			return nil, 0, 0, fmt.Errorf("minimal header received before full header")
		}

	default:
		return nil, 0, 0, fmt.Errorf("unknown magic 0x%04X", magic)
	}

	// Convert big-endian int16 → little-endian int16
	n := len(raw) / 2
	le := make([]byte, len(raw))
	for i := 0; i < n; i++ {
		s := binary.BigEndian.Uint16(raw[i*2:])
		binary.LittleEndian.PutUint16(le[i*2:], s)
	}
	return le, rate, ch, nil
}

func (d *pcmDecoder) close() { d.zd.Close() }

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

type client struct {
	baseURL       string
	password      string
	sessionID     string
	autoReconnect bool
	running       bool
	server        *ScopeServer
	updateHz      int
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
	q.Set("mode", iqMode)
	q.Set("format", "pcm-zstd")
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

	fmt.Fprintf(os.Stderr, "connected — freq=%d Hz, mode=%s, sample rate=%d Hz\n",
		loranFrequencyHz, iqMode, iqSampleRate)

	dec, err := newPCMDecoder()
	if err != nil {
		fmt.Fprintf(os.Stderr, "decoder init: %v\n", err)
		return false
	}
	defer dec.close()

	// Create the Loran-C decoder for this connection.
	loranDec := NewLoranDecoder(float64(iqSampleRate), c.updateHz)
	loranDec.Start()

	// Notify the scope server of the new decoder so the browser can connect.
	c.server.SetDecoder(loranDec)
	defer c.server.SetDecoder(nil)

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

	firstPacket := true

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
			pcmBytes, rate, ch, err := dec.decode(msg, true /* pcm-zstd */)
			if err != nil {
				fmt.Fprintf(os.Stderr, "decode error: %v\n", err)
				continue
			}
			if len(pcmBytes) == 0 {
				continue
			}
			if firstPacket {
				fmt.Fprintf(os.Stderr, "receiving IQ: %d Hz, %d channel(s)\n", rate, ch)
				firstPacket = false
			}

			// Convert raw bytes → []int16 (little-endian, interleaved I/Q).
			nSamples := len(pcmBytes) / 2
			samples := make([]int16, nSamples)
			for i := 0; i < nSamples; i++ {
				samples[i] = int16(binary.LittleEndian.Uint16(pcmBytes[i*2:]))
			}

			// Feed samples to the Loran-C decoder; broadcast any scope frames.
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
		backoff := time.Duration(1<<uint(retries)) * time.Second
		if backoff > maxBackoff {
			backoff = maxBackoff
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
		webPort   = flag.Int("web-port", defaultWebPort, "Port for the Loran-C scope web UI")
		webStatic = flag.String("web-static", "./static", "Path to static web files directory")
		updateHz  = flag.Int("update-hz", 10, "Scope update rate in Hz (1=KiwiSDR-compatible, 10=default, max ~25)")
		noReconn  = flag.Bool("no-reconnect", false, "Disable auto-reconnect on disconnect")
	)
	flag.Parse()

	if *rawURL == "" {
		fmt.Fprintf(os.Stderr, "Usage: ubersdr_loran -url <http://host:port> [-pass <password>] [-web-port <port>] [-web-static <path>] [-update-hz <hz>] [-no-reconnect]\n\n")
		fmt.Fprintf(os.Stderr, "  Connects to UberSDR at 100 kHz (Loran-C carrier) using IQ mode (%d Hz sample rate)\n", iqSampleRate)
		fmt.Fprintf(os.Stderr, "  and serves a Loran-C pulse-envelope scope at http://localhost:%d/\n", defaultWebPort)
		os.Exit(1)
	}

	// Start the scope HTTP/WebSocket server.
	server := NewScopeServer(*webStatic, iqSampleRate, *updateHz)
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
		sessionID:     uuid.New().String(),
		autoReconnect: !*noReconn,
		running:       true,
		server:        server,
		updateHz:      *updateHz,
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
