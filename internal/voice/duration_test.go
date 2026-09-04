package voice

import "testing"

// mpeg1LayerIIISampleRate and friends mirror the spec constants used by the
// parser under test. Tests build raw frame bytes independently of the
// production frameLen formula's *result* (they assert against a duration
// computed straight from sample count and rate) even though they reuse the
// formula to lay out valid frame boundaries.
const (
	testVersionMPEG1 = 3
	testLayerIII     = 1
)

// buildMP3Frame lays out one syntactically valid MPEG1 Layer III frame at the
// given bitrate/sample-rate table indices, with a payload of zero bytes sized
// to exactly match the frame length the parser will compute.
func buildMP3Frame(bitrateIdx, sampleRateIdx, padding int, payloadPrefix []byte) []byte {
	bitrateKbps := mp3Bitrate(testVersionMPEG1, testLayerIII, bitrateIdx)
	sr := mp3SampleRate(testVersionMPEG1, sampleRateIdx)
	samplesPerFrame := mp3SamplesPerFrame(testVersionMPEG1, testLayerIII)
	frameLen := (samplesPerFrame/8)*bitrateKbps*1000/sr + padding

	frame := make([]byte, frameLen)
	frame[0] = 0xFF
	frame[1] = 0xE0 | (testVersionMPEG1 << 3) | (testLayerIII << 1) | 0x1
	frame[2] = byte(bitrateIdx<<4) | byte(sampleRateIdx<<2) | byte(padding<<1)
	frame[3] = 0x00
	copy(frame[4:], payloadPrefix)
	return frame
}

func mp3ExpectedMillis(sampleRateIdx int, frameCount int) int64 {
	sr := int64(mp3SampleRate(testVersionMPEG1, sampleRateIdx))
	samplesPerFrame := int64(mp3SamplesPerFrame(testVersionMPEG1, testLayerIII))
	return int64(frameCount) * samplesPerFrame * 1000 / sr
}

func TestMP3DurationCBR(t *testing.T) {
	const frameCount = 20
	const bitrateIdx, sampleRateIdx = 9, 0 // 128kbps, 44100Hz
	var data []byte
	for i := 0; i < frameCount; i++ {
		data = append(data, buildMP3Frame(bitrateIdx, sampleRateIdx, i%2, nil)...)
	}
	got, ok := mp3DurationMillis(data)
	if !ok {
		t.Fatalf("expected ok=true for a clean CBR stream")
	}
	want := mp3ExpectedMillis(sampleRateIdx, frameCount)
	if got != want {
		t.Fatalf("duration = %dms, want %dms", got, want)
	}
}

func TestMP3DurationVBRWithXingHeader(t *testing.T) {
	const sampleRateIdx = 0 // 44100Hz
	bitrateIdxs := []int{14, 9, 4, 8, 1, 10, 12, 6}
	var data []byte
	for i, idx := range bitrateIdxs {
		var payload []byte
		if i == 0 {
			// Real encoders emit a Xing/Info tag as the payload of the first
			// frame in a VBR stream. The parser does not need to recognize
			// it specially: it is a normal, valid frame and contributes its
			// own duration like any other, which is what is asserted below.
			payload = []byte("Xing")
		}
		data = append(data, buildMP3Frame(idx, sampleRateIdx, i%2, payload)...)
	}
	got, ok := mp3DurationMillis(data)
	if !ok {
		t.Fatalf("expected ok=true for a VBR stream with a Xing header frame")
	}
	want := mp3ExpectedMillis(sampleRateIdx, len(bitrateIdxs))
	if got != want {
		t.Fatalf("duration = %dms, want %dms", got, want)
	}
}

func TestMP3DurationSkipsID3v2Tag(t *testing.T) {
	const frameCount = 5
	const bitrateIdx, sampleRateIdx = 9, 0
	tagBody := make([]byte, 200)
	id3 := append([]byte("ID3"), 0x03, 0x00, 0x00) // version 2.3, no flags
	// Synchsafe size of the tag body (200 bytes -> low 7 bits per byte).
	size := len(tagBody)
	id3 = append(id3, byte(size>>21)&0x7F, byte(size>>14)&0x7F, byte(size>>7)&0x7F, byte(size)&0x7F)
	id3 = append(id3, tagBody...)

	var data []byte
	data = append(data, id3...)
	for i := 0; i < frameCount; i++ {
		data = append(data, buildMP3Frame(bitrateIdx, sampleRateIdx, 0, nil)...)
	}

	got, ok := mp3DurationMillis(data)
	if !ok {
		t.Fatalf("expected ok=true for an ID3v2-prefixed stream")
	}
	want := mp3ExpectedMillis(sampleRateIdx, frameCount)
	if got != want {
		t.Fatalf("duration = %dms, want %dms", got, want)
	}
}

func TestMP3DurationConcatenatedChunks(t *testing.T) {
	// voice.Render concatenates each TTS chunk's raw audio bytes end to end;
	// the parser must sum duration across the seam, including when a few
	// stray bytes separate the two chunks.
	const sampleRateIdx = 0
	chunkA := buildMP3Frame(9, sampleRateIdx, 0, nil)
	chunkA = append(chunkA, buildMP3Frame(9, sampleRateIdx, 1, nil)...)
	chunkB := buildMP3Frame(4, sampleRateIdx, 0, nil)
	chunkB = append(chunkB, buildMP3Frame(4, sampleRateIdx, 1, nil)...)
	chunkB = append(chunkB, buildMP3Frame(4, sampleRateIdx, 0, nil)...)

	combined := append(append(append([]byte{}, chunkA...), 0x00, 0x00, 0x00), chunkB...)

	got, ok := mp3DurationMillis(combined)
	if !ok {
		t.Fatalf("expected ok=true for a concatenated multi-chunk stream")
	}
	want := mp3ExpectedMillis(sampleRateIdx, 5) // 2 frames from chunk A + 3 from chunk B
	if got != want {
		t.Fatalf("duration = %dms, want %dms", got, want)
	}
}

func TestMP3DurationUnparseableReturnsUnknown(t *testing.T) {
	cases := map[string][]byte{
		"empty":           {},
		"too short":       {0xFF, 0xE0},
		"no sync at all":  []byte("this is not an mp3 file at all, just plain text bytes"),
		"random noise":    {0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A},
		"reserved layer":  {0xFF, 0xE0 | (testVersionMPEG1 << 3), 0x00, 0x00}, // layer bits = 00 (reserved)
		"reserved bitidx": {0xFF, 0xE0 | (testVersionMPEG1 << 3) | (testLayerIII << 1), 0xF0, 0x00},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := mp3DurationMillis(data); ok {
				t.Fatalf("expected ok=false for %s", name)
			}
		})
	}
}

func TestWAVDuration(t *testing.T) {
	// 1 second of mono 16-bit PCM at 8000Hz: byteRate = 8000*1*2 = 16000.
	sampleRate := uint32(8000)
	channels := uint16(1)
	bitsPerSample := uint16(16)
	byteRate := sampleRate * uint32(channels) * uint32(bitsPerSample) / 8
	blockAlign := channels * bitsPerSample / 8
	dataSize := byteRate * 2 // 2 seconds

	le32 := func(v uint32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
	le16 := func(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }

	var data []byte
	data = append(data, []byte("RIFF")...)
	data = append(data, le32(36+dataSize)...)
	data = append(data, []byte("WAVE")...)
	data = append(data, []byte("fmt ")...)
	data = append(data, le32(16)...)     // fmt chunk size
	data = append(data, le16(1)...)      // PCM
	data = append(data, le16(channels)...)
	data = append(data, le32(sampleRate)...)
	data = append(data, le32(byteRate)...)
	data = append(data, le16(blockAlign)...)
	data = append(data, le16(bitsPerSample)...)
	data = append(data, []byte("data")...)
	data = append(data, le32(dataSize)...)
	data = append(data, make([]byte, dataSize)...)

	got, ok := wavDurationMillis(data)
	if !ok {
		t.Fatalf("expected ok=true for a canonical WAV file")
	}
	if got != 2000 {
		t.Fatalf("duration = %dms, want 2000ms", got)
	}
}

func TestWAVDurationNotAWav(t *testing.T) {
	if _, ok := wavDurationMillis([]byte("not a wav file")); ok {
		t.Fatalf("expected ok=false for non-WAV bytes")
	}
}

func TestDurationDispatchesByMIMEType(t *testing.T) {
	if _, ok := Duration("audio/opus", []byte{1, 2, 3}); ok {
		t.Fatalf("expected ok=false for an unhandled format")
	}
	if _, ok := Duration("", nil); ok {
		t.Fatalf("expected ok=false for an empty MIME type")
	}
	// A parseable MP3 stream should resolve through the "audio/mpeg; ..." form too.
	data := buildMP3Frame(9, 0, 0, nil)
	if _, ok := Duration("audio/mpeg;codec=mp3lame", data); !ok {
		t.Fatalf("expected ok=true for audio/mpeg with a parameter suffix")
	}
}
