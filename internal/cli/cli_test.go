package cli

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/audio"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	runErr := Run(context.Background(), args, os.Stdin, wOut, wErr)
	_ = wOut.Close()
	_ = wErr.Close()
	outBytes, _ := os.ReadFile(rOut.Name())
	_ = rOut.Close()
	_ = rErr.Close()
	return string(outBytes), runErr
}

func runCLICapture(t *testing.T, args ...string) (string, error) {
	t.Helper()
	tmpOut, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	defer tmpOut.Close()
	tmpErr, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer tmpErr.Close()

	runErr := Run(context.Background(), args, os.Stdin, tmpOut, tmpErr)
	_, _ = tmpOut.Seek(0, 0)
	b, _ := os.ReadFile(tmpOut.Name())
	return string(b), runErr
}

func TestCLIServiceNotRunningAndEndToEndLifecycle(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "eklips.db")
	port := strconv.Itoa(freePort(t))
	baseFlags := []string{"--db", dbPath, "--sink", "null", "--port", port}

	// 1. When daemon is not running, client commands fail with SERVICE_NOT_RUNNING.
	if _, err := runCLICapture(t, append([]string{"status"}, baseFlags...)...); !apperr.Is(err, apperr.ServiceNotRunning) {
		t.Fatalf("status without daemon: got %v, want SERVICE_NOT_RUNNING", err)
	}

	// 2. Start daemon in foreground goroutine (simulating `eklips serve`), drive via CLI, and quit.
	serveDone := make(chan error, 1)
	go func() {
		_, err := runCLICapture(t, append([]string{"serve"}, baseFlags...)...)
		serveDone <- err
	}()

	// Wait until status succeeds.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := runCLICapture(t, append([]string{"status"}, baseFlags...)...); err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	// Create test audio files.
	f1 := filepath.Join(dir, "01.wav")
	f2 := filepath.Join(dir, "02.wav")
	_ = audio.WriteSineWAV(f1, 44100, 44100*2, 440)
	_ = audio.WriteSineWAV(f2, 44100, 44100*2, 880)

	// Play directory, check status, louder, mute, pause, stop, quit.
	if out, err := runCLICapture(t, append([]string{"play", dir}, baseFlags...)...); err != nil || !strings.Contains(out, "playing") {
		t.Fatalf("CLI play dir: out=%q err=%v", out, err)
	}
	if out, err := runCLICapture(t, append([]string{"louder"}, baseFlags...)...); err != nil || !strings.Contains(out, "85%") {
		t.Fatalf("CLI louder: out=%q err=%v", out, err)
	}
	if out, err := runCLICapture(t, append([]string{"next"}, baseFlags...)...); err != nil || !strings.Contains(out, "02.wav") {
		t.Fatalf("CLI next: out=%q err=%v", out, err)
	}
	if out, err := runCLICapture(t, append([]string{"pause"}, baseFlags...)...); err != nil || !strings.Contains(out, "paused") {
		t.Fatalf("CLI pause: out=%q err=%v", out, err)
	}
	if _, err := runCLICapture(t, append([]string{"quit"}, baseFlags...)...); err != nil {
		t.Fatalf("CLI quit: %v", err)
	}

	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for daemon to exit after quit")
	}
}

func TestHelpAndPlaylistCommands(t *testing.T) {
	out, err := runCLICapture(t, "help")
	if err != nil || !strings.Contains(out, "eklips tui") {
		t.Fatalf("help: out=%q err=%v", out, err)
	}
	if _, err := runCLICapture(t, "unknown-cmd"); !apperr.Is(err, apperr.InvalidCommand) {
		t.Fatalf("unknown-cmd: got %v, want INVALID_COMMAND", err)
	}
}
