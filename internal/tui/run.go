package tui

import (
	"context"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/client"
	"github.com/frdn/eklips/internal/config"
	"github.com/frdn/eklips/internal/daemon"
)

// IsTTY reports whether f is an interactive terminal.
func IsTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	fd := f.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

// Run launches the built-in TUI against a local or remote daemon.
// It checks for an interactive TTY BEFORE probing or auto-starting any daemon.
func Run(ctx context.Context, cfg config.Config, in, out *os.File) error {
	if !IsTTY(in) || !IsTTY(out) {
		return apperr.New(apperr.TUIRequiresTTY, "eklips tui requires an interactive terminal (TTY)")
	}

	cl, err := client.FromConfig(cfg)
	if err != nil {
		return err
	}

	startedByUs := false
	probeCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	herr := cl.Health(probeCtx)
	cancel()

	if herr != nil {
		// Never auto-start for --remote or when --no-start is set.
		if cfg.Remote != "" || cfg.NoStart {
			return apperr.New(apperr.ServiceNotRunning, "no daemon reachable at %s", cl.BaseURL())
		}
		var startErr error
		startedByUs, startErr = daemon.StartBackground(ctx, cfg, 5*time.Second)
		if startErr != nil {
			return startErr
		}
	}

	defer func() {
		if cfg.StopOnExit && startedByUs {
			sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = cl.Shutdown(sctx)
			scancel()
		}
	}()

	model := NewModel(ctx, cl)
	if cfg.Theme != "" && !model.SetTheme(cfg.Theme) {
		model.infoBanner = "unknown theme " + cfg.Theme + "; using default"
	}
	defer model.Close()

	p := tea.NewProgram(
		model,
		tea.WithInput(in),
		tea.WithOutput(out),
		tea.WithAltScreen(),
		tea.WithFPS(30),
		tea.WithContext(ctx),
	)

	defer func() {
		if r := recover(); r != nil {
			_ = p.ReleaseTerminal()
			panic(r)
		}
	}()

	if _, err := p.Run(); err != nil {
		_ = p.ReleaseTerminal()
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
