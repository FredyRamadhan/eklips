// Package daemon manages foreground execution (`serve`) and background
// spawning (`start` and TUI auto-start) of the eklips daemon.
package daemon

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/client"
	"github.com/frdn/eklips/internal/config"
	"github.com/frdn/eklips/internal/devwatch"
	"github.com/frdn/eklips/internal/engine"
	"github.com/frdn/eklips/internal/server"
	"github.com/frdn/eklips/internal/store"
)

// RunForeground runs the daemon in the foreground until ctx is cancelled,
// SIGINT/SIGTERM arrives, or /api/v1/shutdown is called.
func RunForeground(ctx context.Context, cfg config.Config) error {
	if err := cfg.ValidateDaemon(); err != nil {
		return err
	}
	// Acquire the single-instance lock BEFORE opening/migrating SQLite so a
	// second daemon fails immediately with ALREADY_RUNNING.
	lockPath := cfg.DBPath + ".lock"
	lock, err := engine.AcquireLock(lockPath)
	if err != nil {
		return err
	}
	defer lock.Release()

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	eng, err := engine.New(engine.Options{
		Store:         st,
		DisableLock:   true, // already locked above
		Sink:          cfg.Sink,
		SampleRate:    cfg.SampleRate,
		MaxListeners:  cfg.MaxListeners,
		MaxQueue:      cfg.MaxQueue,
		MediaRoot:     cfg.MediaRoot,
		ResumeOnStart: cfg.ResumeOnStart,
	})
	if err != nil {
		return err
	}
	defer eng.Close()

	var watcher *devwatch.Watcher
	if cfg.Sink.HasDevice() {
		watcher = devwatch.Start(devwatch.Options{
			OnChange: eng.OnDeviceChange,
		})
		defer watcher.Stop()
	}

	runCtx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	shutdownReq := make(chan struct{}, 1)
	srv := server.New(eng, server.Options{
		Addr:      cfg.Addr(),
		AuthToken: cfg.AuthToken,
		OnShutdown: func() {
			select {
			case shutdownReq <- struct{}{}:
			default:
			}
		},
	})
	if _, err := srv.Start(); err != nil {
		return apperr.Wrap(apperr.ServiceStartFailed, err, "listen on %s", cfg.Addr())
	}

	select {
	case <-runCtx.Done():
	case <-shutdownReq:
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	return nil
}

// StartBackground spawns a detached daemon process using the current executable
// and waits up to timeout (default 5s) for /api/v1/health to respond.
// It returns startedByUs=true when the newly spawned child became the active
// daemon, or startedByUs=false when an existing instance was already running
// or won a startup race.
func StartBackground(ctx context.Context, cfg config.Config, timeout time.Duration) (startedByUs bool, err error) {
	if err := cfg.ValidateDaemon(); err != nil {
		return false, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	cl, err := client.FromConfig(cfg)
	if err != nil {
		return false, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	err = cl.Health(probeCtx)
	cancel()
	if err == nil {
		return false, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return false, apperr.Wrap(apperr.ServiceStartFailed, err, "resolve executable path")
	}

	args := cfg.ForwardedDaemonArgs()
	cmd := exec.Command(exe, args...)
	detachProcess(cmd)
	cmd.Stdin = nil
	cmd.Stdout = nil
	var stderrBuf limitedBuffer
	cmd.Stderr = &stderrBuf

	env := os.Environ()
	if cfg.AuthToken != "" {
		env = setEnv(env, "EKLIPS_TOKEN", cfg.AuthToken)
	}
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		return false, apperr.Wrap(apperr.ServiceStartFailed, err, "spawn background daemon")
	}

	childExit := make(chan error, 1)
	go func() {
		childExit <- cmd.Wait()
	}()

	deadline := time.Now().Add(timeout)
	childExited := false
	var exitErr error
	for time.Now().Before(deadline) {
		select {
		case exitErr = <-childExit:
			childExited = true
		default:
		}

		pctx, pcancel := context.WithTimeout(ctx, 300*time.Millisecond)
		herr := cl.Health(pctx)
		pcancel()
		if herr == nil {
			// If our child exited (e.g. lost single-instance race) but health is up,
			// another instance won the race; attach to it with startedByUs=false.
			if childExited {
				return false, nil
			}
			// Give a brief window to confirm our child didn't just exit due to race.
			select {
			case <-childExit:
				return false, nil
			case <-time.After(25 * time.Millisecond):
				return true, nil
			}
		}

		if childExited {
			// Child exited and health check failed; wait briefly in case a peer
			// winner is still finishing HTTP startup.
			time.Sleep(100 * time.Millisecond)
			pctx2, pcancel2 := context.WithTimeout(ctx, 300*time.Millisecond)
			herr2 := cl.Health(pctx2)
			pcancel2()
			if herr2 == nil {
				return false, nil
			}
			detail := strings.TrimSpace(stderrBuf.String())
			if detail == "" && exitErr != nil {
				detail = exitErr.Error()
			}
			return false, apperr.New(apperr.ServiceStartFailed, "daemon exited before becoming healthy: %s", detail)
		}

		select {
		case <-ctx.Done():
			return false, apperr.Wrap(apperr.ServiceStartFailed, ctx.Err(), "cancelled waiting for daemon")
		case <-time.After(50 * time.Millisecond):
		}
	}

	return false, apperr.New(apperr.ServiceStartFailed, "timed out after %s waiting for daemon health at %s", timeout, cl.BaseURL())
}

type limitedBuffer struct {
	buf []byte
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if len(l.buf) < 4096 {
		rem := 4096 - len(l.buf)
		if len(p) > rem {
			l.buf = append(l.buf, p[:rem]...)
		} else {
			l.buf = append(l.buf, p...)
		}
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string { return string(l.buf) }

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return append(out, prefix+value)
}
