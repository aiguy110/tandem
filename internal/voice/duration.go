package voice

import (
	"encoding/binary"
	"strings"
)

// Duration estimates the playback length of a rendered clip directly from its
// bytes, without any external decoder. It is deliberately conservative: any
// format or stream it cannot parse confidently returns ok=false rather than a
// guessed number, so callers must treat that as "unknown" (typically
// persisted as 0ms) instead of surfacing a wrong duration to the UI.
//
// Only MP3 (MPEG-1/2/2.5 Layer III, the default TTS format) and PCM WAV are
// parsed. Opus, FLAC, AAC, and raw PCM are intentionally left unknown: each
// would need either a container/bitstream parser (Ogg for Opus, box parsing
// for AAC/FLAC) or, for raw PCM, sample-rate/bit-depth metadata this package
// does not have. Extending this function is the place to add them later.
func Duration(mimeType string, data []byte) (ms int64, ok bool) {
	switch normalizeAudioMIME(mimeType) {
	case "audio/mpeg", "audio/mp3":
		return mp3DurationMillis(data)
	case "audio/wav", "audio/x-wav", "audio/wave":
		return wavDurationMillis(data)
	default:
		return 0, false
	}
}

func normalizeAudioMIME(mimeType string) string {
	mimeType = strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0])
	return strings.ToLower(mimeType)
}

// --- MP3 -------------------------------------------------------------------

// mp3DurationMillis sums the duration of every valid MPEG audio frame it can
// find, starting after any ID3v2 tag. This works uniformly for CBR and VBR
// streams (each frame's own header dictates its duration, so no reliance on a
// Xing/Info/VBRI header is required) and for buffers formed by concatenating
// multiple independently-encoded chunks end to end (voice.Render's combined
// buffer): the walk simply keeps consuming frames across the seam, resyncing
// past anything unexpected in between.
func mp3DurationMillis(data []byte) (int64, bool) {
	offset := skipID3v2(data)
	var totalSamples int64
	var sampleRate int
	found := false

	for offset+4 <= len(data) {
		if data[offset] != 0xFF || data[offset+1]&0xE0 != 0xE0 {
			next := indexOfFrameSync(data, offset+1)
			if next < 0 {
				break
			}
			offset = next
			continue
		}
		hdr := data[offset : offset+4]
		version := (hdr[1] >> 3) & 0x3 // 0=MPEG2.5, 1=reserved, 2=MPEG2, 3=MPEG1
		layer := (hdr[1] >> 1) & 0x3   // 0=reserved, 1=LayerIII, 2=LayerII, 3=LayerI
		bitrateIdx := (hdr[2] >> 4) & 0xF
		sampleRateIdx := (hdr[2] >> 2) & 0x3
		padding := int((hdr[2] >> 1) & 0x1)

		if version == 1 || layer == 0 || bitrateIdx == 0 || bitrateIdx == 0xF || sampleRateIdx == 3 {
			next := indexOfFrameSync(data, offset+1)
			if next < 0 {
				break
			}
			offset = next
			continue
		}

		bitrateKbps := mp3Bitrate(version, layer, int(bitrateIdx))
		sr := mp3SampleRate(version, int(sampleRateIdx))
		samplesPerFrame := mp3SamplesPerFrame(version, layer)
		if bitrateKbps <= 0 || sr <= 0 || samplesPerFrame <= 0 {
			next := indexOfFrameSync(data, offset+1)
			if next < 0 {
				break
			}
			offset = next
			continue
		}

		var frameLen int
		if layer == 3 { // Layer I: 4-byte "slots"
			frameLen = (12*bitrateKbps*1000/sr + padding) * 4
		} else { // Layer II/III: 1-byte slots
			frameLen = (samplesPerFrame/8)*bitrateKbps*1000/sr + padding
		}
		if frameLen <= 4 {
			next := indexOfFrameSync(data, offset+1)
			if next < 0 {
				break
			}
			offset = next
			continue
		}

		totalSamples += int64(samplesPerFrame)
		sampleRate = sr
		found = true
		offset += frameLen
	}

	if !found || sampleRate <= 0 {
		return 0, false
	}
	return totalSamples * 1000 / int64(sampleRate), true
}

func indexOfFrameSync(data []byte, start int) int {
	for i := start; i+1 < len(data); i++ {
		if data[i] == 0xFF && data[i+1]&0xE0 == 0xE0 {
			return i
		}
	}
	return -1
}

// skipID3v2 returns the offset immediately following a leading ID3v2 tag, or
// 0 if the buffer does not start with one.
func skipID3v2(data []byte) int {
	if len(data) < 10 || string(data[0:3]) != "ID3" {
		return 0
	}
	size := synchsafeInt(data[6:10])
	offset := 10 + size
	if data[5]&0x10 != 0 { // footer present
		offset += 10
	}
	if offset < 0 || offset > len(data) {
		return len(data)
	}
	return offset
}

func synchsafeInt(b []byte) int {
	return int(b[0])<<21 | int(b[1])<<14 | int(b[2])<<7 | int(b[3])
}

var mp3BitrateMPEG1 = [4][16]int{
	1: {0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, -1}, // Layer I
	2: {0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, -1},    // Layer II
	3: {0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, -1},     // Layer III
}

var mp3BitrateMPEG2 = [4][16]int{
	1: {0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, -1}, // Layer I
	2: {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, -1},      // Layer II
	3: {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, -1},      // Layer III
}

func mp3Bitrate(version, layer byte, idx int) int {
	if idx < 0 || idx > 15 {
		return -1
	}
	if version == 3 { // MPEG1
		return mp3BitrateMPEG1[layer][idx]
	}
	// MPEG2 and MPEG2.5 share a bitrate table.
	return mp3BitrateMPEG2[layer][idx]
}

var mp3SampleRates = map[byte][3]int{
	3: {44100, 48000, 32000}, // MPEG1
	2: {22050, 24000, 16000}, // MPEG2
	0: {11025, 12000, 8000},  // MPEG2.5
}

func mp3SampleRate(version byte, idx int) int {
	rates, ok := mp3SampleRates[version]
	if !ok || idx < 0 || idx > 2 {
		return -1
	}
	return rates[idx]
}

func mp3SamplesPerFrame(version, layer byte) int {
	switch layer {
	case 3: // Layer I
		return 384
	case 2: // Layer II
		return 1152
	case 1: // Layer III
		if version == 3 { // MPEG1
			return 1152
		}
		return 576 // MPEG2 / MPEG2.5
	default:
		return 0
	}
}

// --- WAV ---------------------------------------------------------------

// wavDurationMillis parses a canonical RIFF/WAVE container's "fmt " and
// "data" chunks. It does not attempt WAVE_FORMAT_EXTENSIBLE or compressed
// (non-PCM) WAV variants beyond what byteRate already covers.
func wavDurationMillis(data []byte) (int64, bool) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return 0, false
	}
	offset := 12
	var byteRate uint32
	var dataSize uint32
	haveFmt, haveData := false, false

	for offset+8 <= len(data) {
		id := string(data[offset : offset+4])
		size := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
		body := offset + 8

		switch id {
		case "fmt ":
			if body+16 > len(data) {
				return 0, false
			}
			byteRate = binary.LittleEndian.Uint32(data[body+8 : body+12])
			haveFmt = true
		case "data":
			dataSize = size
			if uint64(body)+uint64(size) > uint64(len(data)) {
				dataSize = uint32(len(data) - body)
			}
			haveData = true
		}

		next := body + int(size) + int(size&1) // chunks are word-aligned
		if next <= offset {
			break
		}
		offset = next
		if haveFmt && haveData {
			break
		}
	}

	if !haveFmt || !haveData || byteRate == 0 {
		return 0, false
	}
	return int64(dataSize) * 1000 / int64(byteRate), true
}
