package clogs

import (
	"testing"

	"github.com/alecthomas/assert/v2"
)

func TestLogger(t *testing.T) {
	logger := NewLogger(LogConfig{Trace: true})
	logger.Debugf("Hello, world!")
	err := logger.Exec(`printf "Hello, world!\n"`)
	assert.NoError(t, err)
}

func TestGitClone(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	logger := NewLogger(LogConfig{Trace: true})
	err := logger.Exec("git clone https://github.com/alecthomas/clogs.git")
	assert.NoError(t, err)
}
