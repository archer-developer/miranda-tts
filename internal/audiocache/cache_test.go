package audiocache

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKey_DiffersByModelVoiceAndText(t *testing.T) {
	base := Key("m", "v", "t")
	assert.NotEqual(t, base, Key("m2", "v", "t"))
	assert.NotEqual(t, base, Key("m", "v2", "t"))
	assert.NotEqual(t, base, Key("m", "v", "t2"))
	assert.True(t, FilenamePattern.MatchString(base+"."+Ext))
}

func TestFilenamePattern_RejectsTraversal(t *testing.T) {
	assert.False(t, FilenamePattern.MatchString("../../etc/passwd"))
	assert.False(t, FilenamePattern.MatchString(Key("m", "v", "t")+".mp3"))
}

func TestCache_StoreThenHasAndDuration(t *testing.T) {
	c, err := New(t.TempDir())
	require.NoError(t, err)
	key := Key("m", "v", "t")
	assert.False(t, c.Has(key))

	require.NoError(t, c.Store(key, []byte("wav"), 1234))

	assert.True(t, c.Has(key))
	ms, ok := c.Duration(key)
	assert.True(t, ok)
	assert.Equal(t, 1234, ms)
}

func TestWrapWAV_HeaderDescribesPayload(t *testing.T) {
	pcm := make([]byte, PCMSampleRate*2) // exactly one second
	wav := WrapWAV(pcm)

	assert.Equal(t, "RIFF", string(wav[0:4]))
	assert.Equal(t, "WAVE", string(wav[8:12]))
	assert.Equal(t, uint32(PCMSampleRate), binary.LittleEndian.Uint32(wav[24:28]))
	assert.Equal(t, uint32(len(pcm)), binary.LittleEndian.Uint32(wav[40:44]))
	assert.Len(t, wav, 44+len(pcm))
	assert.Equal(t, 1000, PCMDurationMS(pcm))
}
