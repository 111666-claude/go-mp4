package rotate

import (
	"bytes"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/abema/go-mp4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRotateFile(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in.mp4")
	output := filepath.Join(dir, "out.mp4")

	orig, err := ioutil.ReadFile("../../../../testdata/sample_dual.mp4")
	require.NoError(t, err)
	require.NoError(t, ioutil.WriteFile(input, orig, 0o600))

	require.Equal(t, 0, Main([]string{"-track-id", "1", "-rotation", "90", input, output}))

	written, err := ioutil.ReadFile(output)
	require.NoError(t, err)
	require.Len(t, written, len(orig))

	rotation, err := mp4.ReadTrackRotation(bytes.NewReader(written), 1)
	require.NoError(t, err)
	assert.Equal(t, mp4.Rotation90, rotation)

	// The input must be left untouched.
	unchanged, err := ioutil.ReadFile(input)
	require.NoError(t, err)
	assert.Equal(t, orig, unchanged)
}

func TestRotateValidation(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in.mp4")
	output := filepath.Join(dir, "out.mp4")
	orig, err := ioutil.ReadFile("../../../../testdata/sample_dual.mp4")
	require.NoError(t, err)
	require.NoError(t, ioutil.WriteFile(input, orig, 0o600))

	// invalid arguments
	assert.NotZero(t, Main([]string{}))
	assert.NotZero(t, Main([]string{"-track-id", "1", "-rotation", "90", input}))
	assert.NotZero(t, Main([]string{"-track-id", "1", "-rotation", "45", input, output}))
	assert.NotZero(t, Main([]string{"-track-id", "7", "-rotation", "90", input, output}))
	assert.NotZero(t, Main([]string{"-track-id", "2", "-rotation", "90", input, output}))
	assert.NotZero(t, Main([]string{"-track-id", "1", "-rotation", "90", filepath.Join(dir, "missing.mp4"), output}))

	// No output must be produced on failure.
	_, err = os.Stat(output)
	assert.True(t, os.IsNotExist(err))

	// A successful run does produce the output.
	assert.Zero(t, Main([]string{"-track-id", "1", "-rotation", "180", input, output}))
	_, err = os.Stat(output)
	require.NoError(t, err)

	// Existing output is replaced atomically.
	require.NoError(t, ioutil.WriteFile(output, []byte("stale"), 0o600))
	assert.Zero(t, Main([]string{"-track-id", "1", "-rotation", "270", input, output}))
	written, err := ioutil.ReadFile(output)
	require.NoError(t, err)
	require.Len(t, written, len(orig))
}
