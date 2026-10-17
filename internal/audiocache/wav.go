package audiocache

import (
	"bytes"
	"encoding/binary"
)

// Gemini's speech API always returns 16-bit signed little-endian mono PCM at
// 24 kHz regardless of model or voice, so these are constants rather than
// values parsed from a response.
const (
	PCMSampleRate    = 24000
	PCMBitsPerSample = 16
	PCMChannels      = 1
)

// PCMDurationMS returns how long pcm plays for, in milliseconds.
func PCMDurationMS(pcm []byte) int { return PCMBytesDurationMS(len(pcm)) }

// PCMBytesDurationMS returns how long n bytes of PCM play for, in
// milliseconds.
func PCMBytesDurationMS(n int) int {
	samples := n / (PCMBitsPerSample / 8 * PCMChannels)
	return samples * 1000 / PCMSampleRate
}

// WrapWAV prepends a standard 44-byte RIFF/WAVE header describing the fixed
// PCM format above to pcm. It only describes the bytes, never re-encodes.
func WrapWAV(pcm []byte) []byte {
	const (
		riffHeaderChunkSize = 36 // bytes after the RIFF size field, before the data payload
		fmtChunkSize        = 16
		pcmAudioFormat      = 1 // WAVE_FORMAT_PCM
	)
	byteRate := PCMSampleRate * PCMChannels * PCMBitsPerSample / 8
	blockAlign := PCMChannels * PCMBitsPerSample / 8

	var buf bytes.Buffer
	buf.Grow(44 + len(pcm))
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(riffHeaderChunkSize+len(pcm)))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(fmtChunkSize))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(pcmAudioFormat))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(PCMChannels))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(PCMSampleRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(byteRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(PCMBitsPerSample))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(pcm)))
	buf.Write(pcm)
	return buf.Bytes()
}
