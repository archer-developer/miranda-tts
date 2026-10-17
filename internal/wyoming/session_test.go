package wyoming

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSynth struct {
	pieces            [][]byte
	err               error
	gotText, gotVoice string
}

func (f *fakeSynth) SynthesizeStream(_ context.Context, text, voice string, emit func([]byte) error) error {
	f.gotText, f.gotVoice = text, voice
	for _, p := range f.pieces {
		if err := emit(p); err != nil {
			return err
		}
	}
	return f.err
}

type testClient struct {
	conn net.Conn
	r    *Reader
	w    *Writer
}

func startServer(t *testing.T, synth Synthesizer) *testClient {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := NewServer("127.0.0.1:0", logger, synth, Service{Model: "m", Voice: "Kore", Languages: []string{"ru", "en"}})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	conn, err := net.Dial("tcp", srv.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	return &testClient{conn: conn, r: NewReader(conn), w: NewWriter(conn)}
}

// next reads one event plus its payload.
func (c *testClient) next(t *testing.T) (Header, []byte) {
	t.Helper()
	h, err := c.r.ReadHeader()
	require.NoError(t, err)
	var payload []byte
	if h.PayloadLength > 0 {
		payload, err = c.r.ReadPayload(h.PayloadLength)
		require.NoError(t, err)
	}
	return h, payload
}

func TestDescribe_AdvertisesConfiguredVoiceAndLanguages(t *testing.T) {
	c := startServer(t, &fakeSynth{})
	require.NoError(t, c.w.WriteMessage(TypeDescribe, nil, nil))

	h, _ := c.next(t)

	require.Equal(t, TypeInfo, h.Type)
	var info InfoData
	require.NoError(t, DecodeData(h, &info))
	require.Len(t, info.TTS, 1)
	prog := info.TTS[0]
	assert.True(t, prog.Installed)
	require.Len(t, prog.Voices, 1)
	assert.Equal(t, "Kore", prog.Voices[0].Name)
	assert.Equal(t, []string{"ru", "en"}, prog.Voices[0].Languages)
	assert.True(t, prog.Voices[0].Installed)
}

func TestSynthesize_OneShotStreamsAudioStartChunksStop(t *testing.T) {
	synth := &fakeSynth{pieces: [][]byte{{1, 2}, {3, 4}}}
	c := startServer(t, synth)
	require.NoError(t, c.w.WriteMessage(TypeSynthesize, SynthesizeData{Text: "привет", Voice: &SynthesizeVoice{Name: "Puck"}}, nil))

	h, _ := c.next(t)
	assert.Equal(t, TypeAudioStart, h.Type)
	var f AudioFormatData
	require.NoError(t, DecodeData(h, &f))
	assert.Equal(t, AudioFormatData{Rate: 24000, Width: 2, Channels: 1}, f)

	h, p := c.next(t)
	assert.Equal(t, TypeAudioChunk, h.Type)
	assert.Equal(t, []byte{1, 2}, p)
	_, p = c.next(t)
	assert.Equal(t, []byte{3, 4}, p)
	h, _ = c.next(t)
	assert.Equal(t, TypeAudioStop, h.Type)

	assert.Equal(t, "привет", synth.gotText)
	assert.Equal(t, "Puck", synth.gotVoice)
}

func TestStreamingHandshake_AccumulatesChunksIgnoresDuplicateAndEndsWithStopped(t *testing.T) {
	synth := &fakeSynth{pieces: [][]byte{{9}}}
	c := startServer(t, synth)
	require.NoError(t, c.w.WriteMessage(TypeSynthesizeStart, SynthesizeData{Voice: &SynthesizeVoice{Name: "Charon"}}, nil))
	require.NoError(t, c.w.WriteMessage(TypeSynthesizeChunk, SynthesizeChunkData{Text: "Привет, "}, nil))
	require.NoError(t, c.w.WriteMessage(TypeSynthesizeChunk, SynthesizeChunkData{Text: "мир."}, nil))
	require.NoError(t, c.w.WriteMessage(TypeSynthesize, SynthesizeData{Text: "Привет, мир."}, nil))
	require.NoError(t, c.w.WriteMessage(TypeSynthesizeStop, nil, nil))

	var types []string
	for {
		h, _ := c.next(t)
		types = append(types, h.Type)
		if h.Type == TypeSynthesizeStopped {
			break
		}
	}

	assert.Equal(t, []string{TypeAudioStart, TypeAudioChunk, TypeAudioStop, TypeSynthesizeStopped}, types)
	assert.Equal(t, "Привет, мир.", synth.gotText, "synthesized once, from the chunks")
	assert.Equal(t, "Charon", synth.gotVoice)
}

func TestSynthesize_FailureIsReportedAsErrorEventAndSessionSurvives(t *testing.T) {
	c := startServer(t, &fakeSynth{err: errors.New("quota")})
	require.NoError(t, c.w.WriteMessage(TypeSynthesize, SynthesizeData{Text: "x"}, nil))

	h, _ := c.next(t)
	assert.Equal(t, TypeError, h.Type)

	require.NoError(t, c.w.WriteMessage(TypeDescribe, nil, nil))
	h, _ = c.next(t)
	assert.Equal(t, TypeInfo, h.Type)
}

func TestUnknownEventWithPayloadDoesNotDesyncStream(t *testing.T) {
	c := startServer(t, &fakeSynth{})
	require.NoError(t, c.w.WriteMessage("mystery", nil, []byte("binary\npayload")))
	require.NoError(t, c.w.WriteMessage(TypeDescribe, nil, nil))

	h, _ := c.next(t)

	assert.Equal(t, TypeInfo, h.Type)
}

func TestSynthesize_EmptyTextYieldsWellFormedEmptyClip(t *testing.T) {
	synth := &fakeSynth{}
	c := startServer(t, synth)
	require.NoError(t, c.w.WriteMessage(TypeSynthesize, SynthesizeData{Text: "  "}, nil))

	h, _ := c.next(t)
	assert.Equal(t, TypeAudioStart, h.Type)
	h, _ = c.next(t)
	assert.Equal(t, TypeAudioStop, h.Type)
	assert.Empty(t, synth.gotText)
}
