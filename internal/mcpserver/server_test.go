package mcpserver

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-tts/internal/speaker"
)

type fakeSpeaker struct {
	gotEntity, gotText, gotVoice string
	res                          speaker.Result
	stopped                      bool
	err                          error
}

func (f *fakeSpeaker) Speak(_ context.Context, entityID, text, voice string) (speaker.Result, error) {
	f.gotEntity, f.gotText, f.gotVoice = entityID, text, voice
	return f.res, f.err
}

func (f *fakeSpeaker) Stop(_ context.Context, entityID string) (bool, error) {
	f.gotEntity = entityID
	return f.stopped, f.err
}

func TestNew_ReturnsServer(t *testing.T) {
	assert.NotNil(t, New(nil, &fakeSpeaker{}))
}

func TestSpeakHandler_RejectsEmptyInput(t *testing.T) {
	h := speakHandler(nil, &fakeSpeaker{})
	_, _, err := h(context.Background(), nil, SpeakInput{Text: "  ", EntityID: "media_player.x"})
	assert.Error(t, err)
	_, _, err = h(context.Background(), nil, SpeakInput{Text: "hi", EntityID: " "})
	assert.Error(t, err)
}

func TestSpeakHandler_PassesTrimmedEntityAndReturnsResult(t *testing.T) {
	sp := &fakeSpeaker{res: speaker.Result{Played: true, DurationMS: 1500}}
	h := speakHandler(slogDiscard(), sp)

	result, out, err := h(context.Background(), nil, SpeakInput{Text: "hi", EntityID: " media_player.x ", Voice: " Puck "})

	require.NoError(t, err)
	assert.Equal(t, "media_player.x", sp.gotEntity)
	assert.Equal(t, "Puck", sp.gotVoice)
	assert.Equal(t, SpeakOutput{Played: true, DurationMS: 1500}, out)
	require.Len(t, result.Content, 1)
}

func TestSpeakHandler_WrapsSpeakerError(t *testing.T) {
	h := speakHandler(slogDiscard(), &fakeSpeaker{err: errors.New("boom")})
	_, _, err := h(context.Background(), nil, SpeakInput{Text: "hi", EntityID: "media_player.x"})
	assert.ErrorContains(t, err, "boom")
}

func TestStopHandler_ReportsStopped(t *testing.T) {
	sp := &fakeSpeaker{stopped: true}
	_, out, err := stopHandler(slogDiscard(), sp)(context.Background(), nil, StopInput{EntityID: "media_player.x"})
	require.NoError(t, err)
	assert.True(t, out.Stopped)
	assert.Equal(t, "media_player.x", sp.gotEntity)
}

func TestStopHandler_RejectsEmptyEntity(t *testing.T) {
	_, _, err := stopHandler(slogDiscard(), &fakeSpeaker{})(context.Background(), nil, StopInput{})
	assert.Error(t, err)
}
