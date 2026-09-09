package mp4

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"github.com/deepch/vdk/av"
	"github.com/deepch/vdk/codec/h264parser"
)

// bitWriter writes an H.264 RBSP bit by bit, which is enough to synthesize the
// small subset of an SPS the parser cares about.
type bitWriter struct {
	buf  []byte
	nbit uint
}

func (w *bitWriter) writeBit(b uint) {
	if w.nbit%8 == 0 {
		w.buf = append(w.buf, 0)
	}
	if b != 0 {
		w.buf[len(w.buf)-1] |= 1 << (7 - w.nbit%8)
	}
	w.nbit++
}

func (w *bitWriter) writeBits(v uint, n uint) {
	for i := n; i > 0; i-- {
		w.writeBit((v >> (i - 1)) & 1)
	}
}

// writeUE writes an unsigned exponential Golomb code.
func (w *bitWriter) writeUE(v uint) {
	v++
	n := uint(0)
	for x := v; x > 1; x >>= 1 {
		n++
	}
	w.writeBits(0, n)
	w.writeBits(v, n+1)
}

// makeSPS builds a baseline profile SPS describing width x height. Only the
// fields ParseSPS reads are emitted, in the order clause 7.3.2.1.1 defines
// them.
func makeSPS(width, height int) []byte {
	mbWidth := (width + 15) / 16
	mbHeight := (height + 15) / 16
	cropRight := (mbWidth*16 - width) / 2
	cropBottom := (mbHeight*16 - height) / 2

	w := &bitWriter{}
	w.writeBits(0x67, 8) // nal_ref_idc=3, nal_unit_type=7 (SPS)
	w.writeBits(66, 8)   // profile_idc: baseline, so no chroma_format_idc block
	w.writeBits(0, 8)    // constraint_set flags + reserved
	w.writeBits(40, 8)   // level_idc
	w.writeUE(0)         // seq_parameter_set_id
	w.writeUE(0)         // log2_max_frame_num_minus4
	w.writeUE(0)         // pic_order_cnt_type
	w.writeUE(0)         // log2_max_pic_order_cnt_lsb_minus4
	w.writeUE(1)         // max_num_ref_frames
	w.writeBit(0)        // gaps_in_frame_num_value_allowed_flag
	w.writeUE(uint(mbWidth - 1))
	w.writeUE(uint(mbHeight - 1))
	w.writeBit(1) // frame_mbs_only_flag
	w.writeBit(1) // direct_8x8_inference_flag
	w.writeBit(1) // frame_cropping_flag
	w.writeUE(0)  // frame_crop_left_offset
	w.writeUE(uint(cropRight))
	w.writeUE(0) // frame_crop_top_offset
	w.writeUE(uint(cropBottom))
	w.writeBit(0) // vui_parameters_present_flag
	w.writeBit(1) // rbsp_stop_one_bit

	return w.buf
}

func makeCodec(t *testing.T, width, height int) h264parser.CodecData {
	t.Helper()

	pps := []byte{0x68, 0xce, 0x38, 0x80}

	cd, err := h264parser.NewCodecDataFromSPSAndPPS(makeSPS(width, height), pps)
	if err != nil {
		t.Fatalf("build codec data for %dx%d: %v", width, height, err)
	}
	if cd.Width() != width || cd.Height() != height {
		t.Fatalf("synthesized SPS decodes as %dx%d, want %dx%d",
			cd.Width(), cd.Height(), width, height)
	}
	return cd
}

func writePackets(t *testing.T, muxer *Muxer, n int, start time.Duration) time.Duration {
	t.Helper()

	tm := start
	for i := 0; i < n; i++ {
		pkt := av.Packet{
			Idx:        0,
			IsKeyFrame: i == 0,
			Time:       tm,
			Data:       []byte{0, 0, 0, 1, byte(i)},
		}
		if err := muxer.WritePacket(pkt); err != nil {
			t.Fatalf("WritePacket %d: %v", i, err)
		}
		tm += 40 * time.Millisecond
	}
	return tm
}

// mux runs build against a Muxer writing to a temp file and returns the bytes
// of the finished file. The result is inspected with the raw box readers below
// rather than mp4io: SampleDesc.Unmarshal keeps only the last avc1 entry, which
// is exactly the detail these tests are about.
func mux(t *testing.T, build func(*Muxer)) []byte {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "mux*.mp4")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer f.Close()

	muxer := NewMuxer(f)
	build(muxer)
	if err := muxer.WriteTrailer(); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return data
}

// boxBody walks a chain of box types starting from a buffer of sibling boxes
// and returns the payload of the last one, header excluded.
func boxBody(t *testing.T, b []byte, path ...string) []byte {
	t.Helper()

	for _, want := range path {
		var found []byte
		for off := 0; off+8 <= len(b); {
			size := int(binary.BigEndian.Uint32(b[off:]))
			if size < 8 || off+size > len(b) {
				t.Fatalf("box %q: bad size %d at offset %d", want, size, off)
			}
			if string(b[off+4:off+8]) == want {
				found = b[off+8 : off+size]
				break
			}
			off += size
		}
		if found == nil {
			t.Fatalf("box %q not found", want)
		}
		b = found
	}
	return b
}

func stblBody(t *testing.T, file []byte) []byte {
	t.Helper()
	return boxBody(t, file, "moov", "trak", "mdia", "minf", "stbl")
}

type sampleEntry struct {
	width  int
	height int
	conf   []byte
}

// avc1Entries reads every avc1 sample entry out of an stsd box, in file order.
func avc1Entries(t *testing.T, file []byte) []sampleEntry {
	t.Helper()

	stsd := boxBody(t, stblBody(t, file), "stsd")
	if len(stsd) < 8 {
		t.Fatalf("stsd too short")
	}
	count := int(binary.BigEndian.Uint32(stsd[4:]))
	entries := stsd[8:]

	var out []sampleEntry
	for off := 0; off+8 <= len(entries); {
		size := int(binary.BigEndian.Uint32(entries[off:]))
		if size < 8 || off+size > len(entries) {
			t.Fatalf("stsd: bad entry size %d at offset %d", size, off)
		}
		if string(entries[off+4:off+8]) == "avc1" {
			// VisualSampleEntry: 78 bytes of fixed fields before the children,
			// with width/height at offset 24 of the payload.
			body := entries[off+8 : off+size]
			if len(body) < 78 {
				t.Fatalf("stsd: avc1 entry too short")
			}
			out = append(out, sampleEntry{
				width:  int(binary.BigEndian.Uint16(body[24:])),
				height: int(binary.BigEndian.Uint16(body[26:])),
				conf:   boxBody(t, body[78:], "avcC"),
			})
		}
		off += size
	}

	if len(out) != count {
		t.Fatalf("stsd declares %d entries but holds %d avc1 entries", count, len(out))
	}
	return out
}

type chunkEntry struct {
	firstChunk      uint32
	samplesPerChunk uint32
	sampleDescIndex uint32
}

func stscEntries(t *testing.T, file []byte) []chunkEntry {
	t.Helper()

	stsc := boxBody(t, stblBody(t, file), "stsc")
	count := int(binary.BigEndian.Uint32(stsc[4:]))
	out := make([]chunkEntry, 0, count)
	for i := 0; i < count; i++ {
		off := 8 + i*12
		if off+12 > len(stsc) {
			t.Fatalf("stsc: declares %d entries but is only %d bytes", count, len(stsc))
		}
		out = append(out, chunkEntry{
			firstChunk:      binary.BigEndian.Uint32(stsc[off:]),
			samplesPerChunk: binary.BigEndian.Uint32(stsc[off+4:]),
			sampleDescIndex: binary.BigEndian.Uint32(stsc[off+8:]),
		})
	}
	return out
}

// trackSize reads the tkhd display size, stored as 16.16 fixed point.
func trackSize(t *testing.T, file []byte) (int, int) {
	t.Helper()

	tkhd := boxBody(t, file, "moov", "trak", "tkhd")
	if tkhd[0] != 0 {
		t.Fatalf("tkhd version %d not handled by this test", tkhd[0])
	}
	if len(tkhd) < 84 {
		t.Fatalf("tkhd too short: %d bytes", len(tkhd))
	}
	return int(binary.BigEndian.Uint32(tkhd[76:]) >> 16), int(binary.BigEndian.Uint32(tkhd[80:]) >> 16)
}

func assertStrictlyIncreasingChunks(t *testing.T, entries []chunkEntry) {
	t.Helper()

	for i, entry := range entries {
		if i > 0 && entry.firstChunk <= entries[i-1].firstChunk {
			t.Errorf("stsc entry %d has first_chunk=%d, not greater than the previous %d",
				i+1, entry.firstChunk, entries[i-1].firstChunk)
		}
		if entry.sampleDescIndex == 0 {
			t.Errorf("stsc entry %d has sample_description_index 0", i+1)
		}
	}
}

// The first stsd entry describes the samples written before any UpdateCodec
// call, so it must keep the sequence header the recording started with. It used
// to be rebuilt from Stream.CodecData at WriteTrailer time, which UpdateCodec
// had already overwritten with the last sequence header: every sample of the
// opening segment then decoded against the wrong SPS and came out as garbage
// (gira #343).
func TestUpdateCodecKeepsInitialSampleDesc(t *testing.T) {
	first := makeCodec(t, 1440, 1080)
	second := makeCodec(t, 1920, 1080)

	file := mux(t, func(muxer *Muxer) {
		if err := muxer.WriteHeader([]av.CodecData{first}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		tm := writePackets(t, muxer, 5, 0)
		if err := muxer.UpdateCodec(0, second); err != nil {
			t.Fatalf("UpdateCodec: %v", err)
		}
		writePackets(t, muxer, 5, tm)
	})

	entries := avc1Entries(t, file)
	if len(entries) != 2 {
		t.Fatalf("got %d avc1 sample descriptions, want 2", len(entries))
	}
	if !bytes.Equal(entries[0].conf, first.AVCDecoderConfRecordBytes()) {
		t.Errorf("stsd entry 1 does not carry the initial sequence header\n got %x\nwant %x",
			entries[0].conf, first.AVCDecoderConfRecordBytes())
	}
	if entries[0].width != 1440 || entries[0].height != 1080 {
		t.Errorf("stsd entry 1 is %dx%d, want 1440x1080", entries[0].width, entries[0].height)
	}
	if !bytes.Equal(entries[1].conf, second.AVCDecoderConfRecordBytes()) {
		t.Errorf("stsd entry 2 does not carry the updated sequence header\n got %x\nwant %x",
			entries[1].conf, second.AVCDecoderConfRecordBytes())
	}
	if entries[1].width != 1920 || entries[1].height != 1080 {
		t.Errorf("stsd entry 2 is %dx%d, want 1920x1080", entries[1].width, entries[1].height)
	}

	// tkhd holds one size for the whole track; the largest keeps the wider
	// segment from being cropped by players that do not re-layout.
	if w, h := trackSize(t, file); w != 1920 || h != 1080 {
		t.Errorf("tkhd is %dx%d, want 1920x1080", w, h)
	}

	assertStrictlyIncreasingChunks(t, stscEntries(t, file))
}

// UpdateCodec before the first sample has been written must retarget the stsc
// entry newStream() preseeded rather than append a second one with the same
// first_chunk, which is not a legal sample-to-chunk table.
func TestUpdateCodecBeforeFirstSample(t *testing.T) {
	first := makeCodec(t, 1440, 1080)
	second := makeCodec(t, 1920, 1080)

	file := mux(t, func(muxer *Muxer) {
		if err := muxer.WriteHeader([]av.CodecData{first}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if err := muxer.UpdateCodec(0, second); err != nil {
			t.Fatalf("UpdateCodec: %v", err)
		}
		writePackets(t, muxer, 5, 0)
	})

	entries := stscEntries(t, file)
	assertStrictlyIncreasingChunks(t, entries)

	if len(entries) != 1 {
		t.Fatalf("got %d stsc entries, want 1", len(entries))
	}
	if entries[0].sampleDescIndex != 2 {
		t.Errorf("stsc entry 1 points at sample description %d, want 2",
			entries[0].sampleDescIndex)
	}
}

// Every sample written before UpdateCodec must stay mapped to the sample
// description that was in effect when it was handed to the muxer.
func TestUpdateCodecSwitchesOnTheRightSample(t *testing.T) {
	first := makeCodec(t, 1440, 1080)
	second := makeCodec(t, 1920, 1080)

	const before = 5

	file := mux(t, func(muxer *Muxer) {
		if err := muxer.WriteHeader([]av.CodecData{first}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		tm := writePackets(t, muxer, before, 0)
		if err := muxer.UpdateCodec(0, second); err != nil {
			t.Fatalf("UpdateCodec: %v", err)
		}
		writePackets(t, muxer, 5, tm)
	})

	entries := stscEntries(t, file)
	assertStrictlyIncreasingChunks(t, entries)

	if len(entries) != 2 {
		t.Fatalf("got %d stsc entries, want 2", len(entries))
	}
	if entries[0].sampleDescIndex != 1 {
		t.Errorf("stsc entry 1 points at sample description %d, want 1", entries[0].sampleDescIndex)
	}
	// One sample per chunk: the first sample of the new codec is sample
	// before+1, i.e. chunk before+1.
	if got, want := entries[1].firstChunk, uint32(before+1); got != want {
		t.Errorf("codec switch starts at chunk %d, want %d (the first sample handed over after UpdateCodec)", got, want)
	}
	if entries[1].sampleDescIndex != 2 {
		t.Errorf("stsc entry 2 points at sample description %d, want 2", entries[1].sampleDescIndex)
	}
}
