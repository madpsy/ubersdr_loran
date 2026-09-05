package pcmv4

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// Third conformance fixture: the reduced-depth IQ mode -min-margin asks for.
//
// testdata/pcmv4_scaled.bin is 192 kHz IQ the server coded at a margin of
// 20 dB, so it carries profile 2 -- a shift byte in front of the coded body,
// samples the predictor saw already quantised, and the shift undone only on the
// way out. It covers the paths that exist nowhere in pcmv4_stream.bin: a shift
// that changes as the margin does, a silent packet that carries no shift at
// all, an escape that carries one, and the profile switching to plain IQ and
// back when the margin goes to lossless.
//
// Getting the shift wrong does not fail; it delivers a signal several bits too
// quiet, which on a Loran-C scope is a flatter envelope and a TDOA fix that
// slowly loses its pulses -- exactly the kind of thing only a hash notices.
const pcmv4ScaledExpectedSHA = "7315366ceed3e70552c28d31cde690a14dc66f5244b5a8dc34a5e696f5698ccc"

func TestPCMv4DecodesScaledStream(t *testing.T) {
	packets := readV4FixtureFile(t, "testdata/pcmv4_scaled.bin")
	dec := NewPCMv4StreamDecoder()
	h := sha256.New()

	sawScaled := false
	for i, pkt := range packets {
		if !PCMv4IsHeader(pkt) {
			t.Fatalf("packet %d not recognised as version 4", i)
		}
		if pkt[4]&pcmv4ProfileMask == PredProfileIQScaled {
			sawScaled = true
		}
		pcmLE, rate, channels, _, _, err := dec.DecodePacketLE(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if channels != 2 {
			t.Fatalf("packet %d: %d channels, want interleaved I/Q", i, channels)
		}
		if len(pcmLE) == 0 || len(pcmLE)%(2*channels) != 0 {
			t.Fatalf("packet %d: %d bytes is not whole frames of %d channels", i, len(pcmLE), channels)
		}
		if rate <= 0 {
			t.Fatalf("packet %d: sample rate %d", i, rate)
		}
		h.Write(pcmLE)
	}

	// Guard the premise: a fixture without a profile 2 packet in it would
	// exercise nothing this test exists for.
	if !sawScaled {
		t.Fatal("the fixture carried no scaled packets")
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != pcmv4ScaledExpectedSHA {
		t.Fatalf("decoded samples differ from what the server encoded\n got %s\nwant %s",
			got, pcmv4ScaledExpectedSHA)
	}
}

// A shift the wire format does not allow must be refused rather than applied:
// it would shift a sample past full scale on every value in the packet.
func TestScaledShiftOutOfRangeIsRejected(t *testing.T) {
	packets := readV4FixtureFile(t, "testdata/pcmv4_scaled.bin")

	// The first packet is the stream's resynchronisation point, so it carries
	// its own metadata and a fresh decoder can read it standing alone.
	first := packets[0]
	if first[4]&pcmv4ProfileMask != PredProfileIQScaled || first[4]&pcmv4FlagSilent != 0 {
		t.Fatal("the fixture no longer opens with a non-silent scaled packet")
	}
	if _, _, err := NewPCMv4StreamDecoder().DecodePacket(first); err != nil {
		t.Fatalf("the unmodified packet did not decode: %v", err)
	}

	// The shift is the first byte of the body, which begins where the header
	// ends.
	_, off, err := NewPCMv4HeaderDecoder().Decode(first)
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	bad := append([]byte(nil), first...)
	bad[off] = 16
	if _, _, err := NewPCMv4StreamDecoder().DecodePacket(bad); err == nil {
		t.Fatal("a shift of 16 was accepted; the wire format allows 0-15")
	}
}
