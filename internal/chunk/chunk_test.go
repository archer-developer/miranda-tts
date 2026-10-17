package chunk

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChunk_ShortTextIsOneChunk(t *testing.T) {
	assert.Equal(t, []string{"Привет. Как дела?"}, Chunk("Привет. Как дела?", 100))
}

func TestChunk_PacksSentencesGreedilyAndCutsAtBoundary(t *testing.T) {
	got := Chunk("Раз. Два. Три. Четыре.", 12)
	assert.Equal(t, []string{"Раз. Два.", "Три.", "Четыре."}, got)
}

func TestChunk_NoBoundaryCutsOnWhitespaceWithinLimit(t *testing.T) {
	got := Chunk("aaaa bbbb cccc dddd", 9)
	for _, c := range got {
		assert.LessOrEqual(t, len([]rune(c)), 9)
	}
	assert.Equal(t, "aaaa bbbb cccc dddd", strings.Join(got, " "))
}

func TestChunk_EmptyTextYieldsNothing(t *testing.T) {
	assert.Empty(t, Chunk("   ", 10))
}
