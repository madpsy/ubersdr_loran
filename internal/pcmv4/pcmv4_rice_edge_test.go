package pcmv4

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"testing"
)

// Second conformance fixture: the Rice codeword edge case.
//
// testdata/pcmv4_rice_edge.bin holds a stream containing a Rice codeword whose
// unary run is exactly 63 bits long and is counted out of a full 64-bit
// accumulator, so the decoder shifts by 64. Go defines a shift of 64 as zero;
// C and C++ do not, and the difference is silent -- the accumulator keeps its
// bits, the packet decodes as noise, and the backward-adaptive predictor then
// adapts to that noise. On live IQ it appeared roughly once in a quarter of a
// million packets: often enough to break a receiver within minutes, rare enough
// that a recording of ordinary traffic holds one only by luck.
//
// The expected hash is the one the server, the C++ SoapySDR driver and every
// other port of this decoder agree on.
const pcmv4RiceEdgeExpectedSHA = "83e3d94b509efbf7a212a3e10193b3eb281fe1460cbfeef6aabe474c92a718c7"

// readV4FixtureFile is readV4Fixture for a named fixture. Same container:
// "UV4F", a format byte, a uint32 packet count, then each packet as a uint32
// length and that many bytes.
func readV4FixtureFile(t *testing.T, path string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fixture %s: %v", path, err)
	}
	if len(raw) < 9 || string(raw[:4]) != "UV4F" || raw[4] != 0 {
		t.Fatalf("fixture %s: bad header", path)
	}
	count := int(binary.LittleEndian.Uint32(raw[5:]))
	off := 9

	packets := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		if off+4 > len(raw) {
			t.Fatalf("fixture %s: truncated length at packet %d", path, i)
		}
		n := int(binary.LittleEndian.Uint32(raw[off:]))
		off += 4
		if off+n > len(raw) {
			t.Fatalf("fixture %s: truncated packet %d", path, i)
		}
		packets = append(packets, raw[off:off+n])
		off += n
	}
	if off != len(raw) {
		t.Fatalf("fixture %s: %d trailing bytes", path, len(raw)-off)
	}
	return packets
}

func TestPCMv4DecodesRiceEdgeStream(t *testing.T) {
	packets := readV4FixtureFile(t, "testdata/pcmv4_rice_edge.bin")
	if len(packets) == 0 {
		t.Fatal("fixture carried no packets")
	}

	dec := NewPCMv4StreamDecoder()
	h := sha256.New()
	for i, pkt := range packets {
		if !PCMv4IsHeader(pkt) {
			t.Fatalf("packet %d not recognised as version 4", i)
		}
		pcmLE, _, _, _, _, err := dec.DecodePacketLE(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		h.Write(pcmLE)
	}

	if got := hex.EncodeToString(h.Sum(nil)); got != pcmv4RiceEdgeExpectedSHA {
		t.Fatalf("decoded samples differ from what the server encoded\n got %s\nwant %s",
			got, pcmv4RiceEdgeExpectedSHA)
	}
}
