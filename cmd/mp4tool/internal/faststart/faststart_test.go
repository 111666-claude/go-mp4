package faststart

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleMP4Path = "../../../../testdata/sample.mp4"
const fragmentedMP4Path = "../../../../testdata/sample_fragmented.mp4"

func TestFastStartCommand(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.mp4")

	code := Main([]string{sampleMP4Path, outPath})
	assert.Equal(t, 0, code)

	got, err := os.ReadFile(outPath)
	require.NoError(t, err)
	orig, err := os.ReadFile(sampleMP4Path)
	require.NoError(t, err)
	assert.Equal(t, len(orig), len(got), "length must be preserved")
	// moov must precede mdat in the output but not in the input.
	indexOf := func(data []byte, tag string) int {
		for i := 4; i+4 <= len(data); i++ {
			if string(data[i:i+4]) == tag {
				return i
			}
		}
		return -1
	}
	assert.Greater(t, indexOf(orig, "moov"), indexOf(orig, "mdat"))
	assert.Less(t, indexOf(got, "moov"), indexOf(got, "mdat"))

	// Re-running on the already-fast-start output must succeed and be stable.
	outPath2 := filepath.Join(t.TempDir(), "out2.mp4")
	assert.Equal(t, 0, Main([]string{outPath, outPath2}))
	got2, err := os.ReadFile(outPath2)
	require.NoError(t, err)
	assert.Equal(t, got, got2)
}

func TestFastStartCommandRejectsFragmented(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.mp4")
	code := Main([]string{fragmentedMP4Path, outPath})
	assert.NotEqual(t, 0, code)
	_, err := os.Stat(outPath)
	assert.True(t, os.IsNotExist(err), "no output file should be created on failure")
}

func TestFastStartCommandUsageError(t *testing.T) {
	assert.NotEqual(t, 0, Main(nil))
	assert.NotEqual(t, 0, Main([]string{"only-one-arg"}))
}

func TestFastStartCommandMissingInput(t *testing.T) {
	code := Main([]string{
		filepath.Join(t.TempDir(), "does-not-exist.mp4"),
		filepath.Join(t.TempDir(), "out.mp4"),
	})
	assert.NotEqual(t, 0, code)
}
