package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/madpsy/ubersdr_loran/internal/pcmv4"
)

// Conformance test for this tool's audio protocol version 4 receive path.
//
// testdata/pcmv4_stream.bin is a packet stream the SERVER's encoder produced,
// and pcmv4ExpectedSHA is the SHA-256 of the samples that went into it, little
// endian, exactly as pcmDecoder.decode must render them before they reach the
// Loran-C envelope decoder.
//
// It earns its 90 kB. The version 4 predictor is backward adaptive: the two
// ends derive their filter taps independently from the samples already coded
// and never exchange a coefficient, so any arithmetic difference between this
// decoder and the Go one on the server produces plausible noise rather than an
// error. Nothing short of comparing the samples would catch it -- the scope
// would fill with grass and the TDOA fix would wander, with nothing anywhere
// saying why.
//
// The fixture deliberately covers the case this tool depends on: interleaved
// I/Q with a packet length that varies across the five-second periodic
// resynchronisation, which is what makes the header's sample count necessary.
// It also covers ordinary mono audio, silent packets carrying no body, an
// escape to verbatim samples on incompressible noise, and a sample-rate change.
//
// internal/pcmv4 checks the codec itself against the same fixture. This test
// checks the wrapper on top of it: that decode() hands on the codec's samples
// in the order they arrived (versions 1-3 carried radiod's big-endian samples
// and this file reversed them per packet -- doing that now would silently
// destroy the envelope), and that it reports the sample rate the Loran decoder
// is constructed from.
const pcmv4ExpectedSHA = "ba368c898ae406c5acc806653d9f2dbbfa40086eca3707fda5d77c13948f78d1"

// readV4Fixture returns the packets in testdata/pcmv4_stream.bin.
//
// Layout: "UV4F", a format byte, a uint32 packet count, then each packet as a
// uint32 length and that many bytes.
func readV4Fixture(t *testing.T) [][]byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/pcmv4_stream.bin")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if len(raw) < 9 || string(raw[:4]) != "UV4F" || raw[4] != 0 {
		t.Fatal("fixture: bad header")
	}
	count := int(binary.LittleEndian.Uint32(raw[5:]))
	off := 9

	packets := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		if off+4 > len(raw) {
			t.Fatalf("fixture: truncated length at packet %d", i)
		}
		n := int(binary.LittleEndian.Uint32(raw[off:]))
		off += 4
		if off+n > len(raw) {
			t.Fatalf("fixture: truncated packet %d", i)
		}
		packets = append(packets, raw[off:off+n])
		off += n
	}
	if off != len(raw) {
		t.Fatalf("fixture: %d trailing bytes", len(raw)-off)
	}
	return packets
}

// hashSamples renders int16 samples little endian, the form the expected hash
// was taken over.
func hashSamples(h interface{ Write([]byte) (int, error) }, samples []int16) {
	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	h.Write(buf) //nolint:errcheck // hash.Hash never returns an error
}

func TestPCMDecoderDecodesServerStream(t *testing.T) {
	packets := readV4Fixture(t)
	dec := newPCMDecoder()
	h := sha256.New()

	// Every distinct (rate, channels) the fixture passes through, in order. A
	// decoder that lost the carried-forward metadata could still hash correctly
	// while mislabelling the stream, and the sample rate is what NewLoranDecoder
	// is built from -- it sets µs per bin for every scope trace and every TDOA
	// measurement.
	wantParams := [][2]int{{12000, 1}, {24000, 1}, {48000, 2}}
	var gotParams [][2]int

	var lastWallMs uint64
	for i, pkt := range packets {
		samples, rate, ch, wallMs, err := dec.decode(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if len(samples) == 0 || len(samples)%ch != 0 {
			t.Fatalf("packet %d: %d samples is not whole frames of %d channels", i, len(samples), ch)
		}
		// timing.go reads this as milliseconds since the Unix epoch, so it must
		// advance monotonically across a continuous stream.
		if wallMs < lastWallMs {
			t.Fatalf("packet %d: wall clock went backwards, %d after %d", i, wallMs, lastWallMs)
		}
		lastWallMs = wallMs

		p := [2]int{rate, ch}
		if len(gotParams) == 0 || gotParams[len(gotParams)-1] != p {
			gotParams = append(gotParams, p)
		}
		hashSamples(h, samples)
	}

	if got := hex.EncodeToString(h.Sum(nil)); got != pcmv4ExpectedSHA {
		t.Fatalf("decoded samples differ from what the server encoded\n got %s\nwant %s",
			got, pcmv4ExpectedSHA)
	}
	if len(gotParams) != len(wantParams) {
		t.Fatalf("stream parameters: got %v, want %v", gotParams, wantParams)
	}
	for i := range wantParams {
		if gotParams[i] != wantParams[i] {
			t.Fatalf("stream parameters: got %v, want %v", gotParams, wantParams)
		}
	}
}

// The I/Q path is the only one this tool actually uses: every mode it offers
// ("iq" through "iq384") is two channels, and LoranDecoder.ProcessSamples reads
// the slice as interleaved I,Q pairs. The fixture's stereo section varies its
// packet length across the periodic resynchronisation, which is exactly where a
// decoder that assumed a fixed sample count would start emitting half-frames
// and swap I with Q for the rest of the stream.
func TestDecodedIQIsWholeInterleavedFrames(t *testing.T) {
	packets := readV4Fixture(t)
	dec := newPCMDecoder()

	iqPackets := 0
	lengths := map[int]bool{}
	for i, pkt := range packets {
		samples, rate, ch, _, err := dec.decode(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if ch != 2 {
			continue
		}
		iqPackets++
		lengths[len(samples)] = true

		// Two samples per frame, so an odd count would leave a component of one
		// frame at the head of the next packet and transpose I and Q from there
		// on.
		if len(samples)%2 != 0 {
			t.Fatalf("packet %d: %d samples is not whole I/Q frames", i, len(samples))
		}
		// Both components must be present; a decoder that dropped one would
		// halve the effective rate and double every measured time difference.
		if len(samples)/2 == 0 {
			t.Fatalf("packet %d: no I/Q frames", i)
		}
		if rate <= 0 {
			t.Fatalf("packet %d: sample rate %d", i, rate)
		}
	}

	if iqPackets == 0 {
		t.Fatal("the fixture carried no I/Q packets")
	}
	if len(lengths) < 2 {
		t.Fatalf("the I/Q section had a single packet length %v; it is meant to vary "+
			"across the periodic resynchronisation", lengths)
	}
}

// The decoder is backward adaptive and carries its header baseline and its
// predictor forward, so a reconnect must start a new one. runOnce() does that
// by calling newPCMDecoder() per connection; this pins the property that makes
// it necessary.
//
// The replay below runs over a PREFIX of the fixture, not the whole of it, and
// that is load-bearing. PCMv4StreamDecoder rebuilds its codec whenever the
// packet's profile changes, and the fixture switches profile partway through
// (the I/Q section uses a different one from the audio section). Replaying the
// entire stream through a carried-over decoder therefore crosses a profile
// change on the first packet of the replay, which incidentally rebuilds the
// predictor and reproduces the expected hash: a test written that way passes
// whether the decoder is reset or not, and proves nothing. A prefix that stays
// inside one profile is what makes stale state visible.
func TestDecoderIsResetOnReconnect(t *testing.T) {
	packets := readV4Fixture(t)
	if len(packets) < 50 {
		t.Fatalf("fixture holds %d packets, too few to replay a prefix", len(packets))
	}
	prefix := packets[:50]

	// Guard the premise: if a regenerated fixture ever put a profile change
	// inside this prefix, the replay would stop discriminating and the test
	// would go quietly green.
	profile := prefix[0][4] & 0x07
	for i, pkt := range prefix {
		if pkt[4]&0x07 != profile {
			t.Fatalf("packet %d changes codec profile; the prefix must stay within one "+
				"profile or the replay below cannot detect carried-over state", i)
		}
	}

	sum := func(d *pcmDecoder, pkts [][]byte) string {
		h := sha256.New()
		for i, pkt := range pkts {
			samples, _, _, _, err := d.decode(pkt)
			if err != nil {
				t.Fatalf("packet %d: %v", i, err)
			}
			hashSamples(h, samples)
		}
		return hex.EncodeToString(h.Sum(nil))
	}

	// What a reconnect that builds a fresh decoder produces.
	fresh := sum(newPCMDecoder(), prefix)

	// What a reconnect that reused the previous connection's decoder would
	// produce: the same packets decoded against an adaptation the new stream
	// never generated. It must NOT match -- if it did, the reset would be
	// unobservable and this test could not tell the two apart.
	carried := newPCMDecoder()
	_ = sum(carried, prefix)
	if replayed := sum(carried, prefix); replayed == fresh {
		t.Fatal("a decoder carried across a reconnect produced the same samples as a fresh " +
			"one; this replay cannot detect stale state, so it is not testing anything")
	}

	// A second connection with its own decoder reproduces the first exactly.
	if got := sum(newPCMDecoder(), prefix); got != fresh {
		t.Fatalf("second connection: got %s, want %s", got, fresh)
	}

	// And a fresh decoder holds none of the previous connection's header
	// baseline either: a mid-stream packet, one that omits the metadata the
	// server only re-sends at a resynchronisation point, is refused rather than
	// decoded against a stale rate and timestamp.
	var delta []byte
	for _, pkt := range packets[1:] {
		if len(pkt) > 4 && pkt[4]&(1<<5) == 0 {
			delta = pkt
			break
		}
	}
	if delta == nil {
		t.Fatal("the fixture carried no delta packets")
	}
	if _, _, _, _, err := newPCMDecoder().decode(delta); err == nil {
		t.Fatal("a fresh decoder accepted a mid-stream packet; it kept state across the reconnect")
	}
}

// A server too old for version 4 answers with the zstd-wrapped version 1 shape.
// Saying so is what stops a silent dead stream.
func TestLegacyServerFrameIsReported(t *testing.T) {
	_, _, _, _, err := newPCMDecoder().decode([]byte{0x28, 0xB5, 0x2F, 0xFD, 0x00})
	if err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("a zstd frame gave %v, want a message naming the old server", err)
	}
}

// This tool used to send no version at all, which the server answers with
// version 1 -- its floor, not its current format. The parameter must now be
// present and must ask for 4.
func TestWSURLRequestsProtocolVersion4(t *testing.T) {
	c := &client{
		baseURL:   "http://example.invalid:8080",
		iqMode:    "iq",
		sessionID: "test-session",
	}

	u, err := url.Parse(c.wsURL())
	if err != nil {
		t.Fatalf("wsURL is not a URL: %v", err)
	}
	q := u.Query()
	if !q.Has("version") {
		t.Fatal("the websocket URL carries no version parameter; the server would serve version 1")
	}
	if got := q.Get("version"); got != "4" {
		t.Fatalf("version=%q, want \"4\"", got)
	}
	if pcmv4.ProtocolVersion != 4 {
		t.Fatalf("the vendored decoder speaks version %d", pcmv4.ProtocolVersion)
	}
	// The format name did not change with the version; only the payload inside
	// it did. Asking for anything else would get a different codec.
	if got := q.Get("format"); got != "pcm-zstd" {
		t.Fatalf("format=%q, want \"pcm-zstd\"", got)
	}
	// The IQ mode and centre frequency are what make this a Loran receiver.
	if got := q.Get("mode"); got != "iq" {
		t.Fatalf("mode=%q, want \"iq\"", got)
	}
	if got := q.Get("frequency"); got != "100000" {
		t.Fatalf("frequency=%q, want \"100000\"", got)
	}
}
