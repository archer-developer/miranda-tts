package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-tts/internal/wyoming"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestConfigFilePaths_MissingDirReturnsNilNoError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")

	paths, err := configFilePaths(dir, discardLogger())
	require.NoError(t, err)
	assert.Nil(t, paths)
}

func TestConfigFilePaths_SkipsNonYAMLFilesDistFileAndDirectories(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", "")
	writeFile(t, dir, "config.yaml.dist", "")
	writeFile(t, dir, "notes.txt", "")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "backup.yaml"), 0o755))

	paths, err := configFilePaths(dir, discardLogger())
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join(dir, "config.yaml")}, paths)
}

func TestConfigFilePaths_ReturnsFilesInSortedOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "z.yaml", "")
	writeFile(t, dir, "a.yaml", "")
	writeFile(t, dir, "m.yaml", "")

	paths, err := configFilePaths(dir, discardLogger())
	require.NoError(t, err)
	assert.Equal(t, []string{
		filepath.Join(dir, "a.yaml"),
		filepath.Join(dir, "m.yaml"),
		filepath.Join(dir, "z.yaml"),
	}, paths)
}

func TestConfigFilePaths_UnreadableDirReturnsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits don't apply on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}

	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) // let t.TempDir() clean up afterward

	_, err := configFilePaths(dir, discardLogger())
	assert.Error(t, err)
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

type noSynth struct{}

func (noSynth) SynthesizeStream(context.Context, string, string, func([]byte) error) error {
	return nil
}

func TestServeUntilInterrupted_StopsBothListenersOnCancel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	wy, err := wyoming.NewServer("127.0.0.1:0", logger, noSynth{}, wyoming.Service{Voice: "v"})
	require.NoError(t, err)
	httpServer := &http.Server{Addr: "127.0.0.1:0", Handler: http.NewServeMux()}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveUntilInterrupted(ctx, httpServer, wy, logger) }()

	conn, err := net.DialTimeout("tcp", wy.Addr().String(), 2*time.Second)
	require.NoError(t, err)
	_ = conn.Close()

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("serveUntilInterrupted did not return after cancel")
	}
}

func TestServeUntilInterrupted_HTTPBindFailureStopsWyoming(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	wy, err := wyoming.NewServer("127.0.0.1:0", logger, noSynth{}, wyoming.Service{Voice: "v"})
	require.NoError(t, err)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = busy.Close() }()
	httpServer := &http.Server{Addr: busy.Addr().String(), Handler: http.NewServeMux()}

	done := make(chan error, 1)
	go func() { done <- serveUntilInterrupted(context.Background(), httpServer, wy, logger) }()

	select {
	case err := <-done:
		assert.ErrorContains(t, err, "http:")
	case <-time.After(5 * time.Second):
		t.Fatal("did not return after http bind failure")
	}
}

func TestServeUntilInterrupted_LongLivedConnectionDoesNotMakeShutdownFail(t *testing.T) {
	old := shutdownTimeout
	shutdownTimeout = 200 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = old })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	wy, err := wyoming.NewServer("127.0.0.1:0", logger, noSynth{}, wyoming.Service{Voice: "v"})
	require.NoError(t, err)

	// Stands in for Miranda's persistent MCP stream: a request that never ends
	// on its own.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-release
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	httpServer := &http.Server{Addr: ln.Addr().String(), Handler: mux}
	require.NoError(t, ln.Close())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveUntilInterrupted(ctx, httpServer, wy, logger) }()

	var resp *http.Response
	require.Eventually(t, func() bool {
		resp, err = http.Get("http://" + httpServer.Addr + "/stream")
		return err == nil
	}, 2*time.Second, 20*time.Millisecond)
	defer func() { _ = resp.Body.Close() }()
	<-started

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err, "a stream that outlives the shutdown deadline must not turn the stop into a failure")
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}
