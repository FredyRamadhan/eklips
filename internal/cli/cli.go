// Package cli parses command-line arguments and dispatches lifecycle, TUI, and
// playback control commands.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/client"
	"github.com/frdn/eklips/internal/config"
	"github.com/frdn/eklips/internal/daemon"
	"github.com/frdn/eklips/internal/engine"
	"github.com/frdn/eklips/internal/sink"
	"github.com/frdn/eklips/internal/tui"
)

type parsedFlags struct {
	cfg    config.Config
	loop   bool
	force  bool
	output string
	help   bool
	args   []string
}

func parseArgs(raw []string) (parsedFlags, error) {
	p := parsedFlags{cfg: config.Default()}
	for i := 0; i < len(raw); i++ {
		arg := raw[i]
		if arg == "--" {
			p.args = append(p.args, raw[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			p.args = append(p.args, arg)
			continue
		}

		name, val, hasEq := strings.Cut(arg, "=")
		nextVal := func() (string, error) {
			if hasEq {
				return val, nil
			}
			if i+1 >= len(raw) {
				return "", apperr.New(apperr.InvalidCommand, "option %s requires a value", name)
			}
			i++
			return raw[i], nil
		}

		switch name {
		case "-h", "--help":
			p.help = true
		case "--db":
			v, err := nextVal()
			if err != nil {
				return p, err
			}
			p.cfg.DBPath = v
		case "--sink":
			v, err := nextVal()
			if err != nil {
				return p, err
			}
			k, ok := sink.ParseKind(v)
			if !ok {
				return p, apperr.New(apperr.InvalidCommand, "invalid --sink %q (expected device, http, both, or null)", v)
			}
			p.cfg.Sink = k
		case "--bind":
			v, err := nextVal()
			if err != nil {
				return p, err
			}
			p.cfg.Bind = v
		case "--port":
			v, err := nextVal()
			if err != nil {
				return p, err
			}
			port, err := strconv.Atoi(v)
			if err != nil || port < 0 || port > 65535 {
				return p, apperr.New(apperr.InvalidCommand, "invalid --port %q", v)
			}
			p.cfg.Port = port
		case "--auth-token":
			v, err := nextVal()
			if err != nil {
				return p, err
			}
			p.cfg.AuthToken = v
		case "--allow-remote":
			p.cfg.AllowRemote = true
		case "--resume-on-start":
			p.cfg.ResumeOnStart = true
		case "--media-root":
			v, err := nextVal()
			if err != nil {
				return p, err
			}
			p.cfg.MediaRoot = v
		case "--sample-rate":
			v, err := nextVal()
			if err != nil {
				return p, err
			}
			sr, err := strconv.Atoi(v)
			if err != nil || sr < 8000 || sr > 192000 {
				return p, apperr.New(apperr.InvalidCommand, "invalid --sample-rate %q", v)
			}
			p.cfg.SampleRate = sr
		case "--remote":
			v, err := nextVal()
			if err != nil {
				return p, err
			}
			p.cfg.Remote = v
		case "--no-start":
			p.cfg.NoStart = true
		case "--stop-on-exit":
			p.cfg.StopOnExit = true
		case "--theme":
			v, err := nextVal()
			if err != nil {
				return p, err
			}
			p.cfg.Theme = v
		case "--loop":
			p.loop = true
		case "--force", "-f":
			p.force = true
		case "--output", "-o":
			v, err := nextVal()
			if err != nil {
				return p, err
			}
			p.output = v
		default:
			return p, apperr.New(apperr.InvalidCommand, "unknown option %q", name)
		}
	}
	return p, nil
}

// Run executes the CLI command in args, writing output to stdout/stderr.
func Run(ctx context.Context, args []string, stdin, stdout, stderr *os.File) error {
	p, err := parseArgs(args)
	if err != nil {
		return err
	}
	if p.help {
		printHelp(stdout)
		return nil
	}

	cmd := "serve"
	var rest []string
	if len(p.args) > 0 {
		cmd = p.args[0]
		rest = p.args[1:]
	}

	switch cmd {
	case "help":
		printHelp(stdout)
		return nil
	case "version":
		fmt.Fprintf(stdout, "eklips %s\n", config.Version)
		return nil
	case "serve":
		if len(rest) > 0 {
			return apperr.New(apperr.InvalidCommand, "unexpected arguments for serve")
		}
		return daemon.RunForeground(ctx, p.cfg)
	case "start":
		if len(rest) > 0 {
			return apperr.New(apperr.InvalidCommand, "unexpected arguments for start")
		}
		started, err := daemon.StartBackground(ctx, p.cfg, 5*time.Second)
		if err != nil {
			return err
		}
		u, _ := p.cfg.BaseURL()
		if started {
			fmt.Fprintf(stdout, "Started eklips daemon at %s\n", u)
		} else {
			fmt.Fprintf(stdout, "eklips daemon is already running at %s\n", u)
		}
		return nil
	case "tui":
		if len(rest) > 0 {
			return apperr.New(apperr.InvalidCommand, "unexpected arguments for tui")
		}
		return tui.Run(ctx, p.cfg, stdin, stdout)
	}

	cl, err := client.FromConfig(p.cfg)
	if err != nil {
		return err
	}

	switch cmd {
	case "quit":
		if len(rest) > 0 {
			return apperr.New(apperr.InvalidCommand, "unexpected arguments for quit")
		}
		if err := cl.Shutdown(ctx); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Stopped eklips daemon")
		return nil

	case "status":
		if len(rest) > 0 {
			return apperr.New(apperr.InvalidCommand, "unexpected arguments for status")
		}
		st, err := cl.Status(ctx)
		if err != nil {
			return err
		}
		printStatus(stdout, st)
		return nil

	case "queue":
		if len(rest) > 0 {
			return apperr.New(apperr.InvalidCommand, "unexpected arguments for queue")
		}
		q, err := cl.Queue(ctx)
		if err != nil {
			return err
		}
		printQueue(stdout, q)
		return nil

	case "play":
		if len(rest) > 1 {
			return apperr.New(apperr.InvalidCommand, "usage: eklips play [<path>] [--loop]")
		}
		var path string
		if len(rest) == 1 {
			abs, err := filepath.Abs(rest[0])
			if err != nil {
				return apperr.Wrap(apperr.InvalidCommand, err, "resolve path")
			}
			path = abs
		}
		st, err := cl.Play(ctx, path, p.loop)
		if err != nil {
			return err
		}
		printStatus(stdout, st)
		return nil

	case "pause":
		if len(rest) > 0 {
			return apperr.New(apperr.InvalidCommand, "unexpected arguments for pause")
		}
		st, err := cl.Pause(ctx)
		if err != nil {
			return err
		}
		printStatus(stdout, st)
		return nil

	case "stop":
		if len(rest) > 0 {
			return apperr.New(apperr.InvalidCommand, "unexpected arguments for stop")
		}
		st, err := cl.Stop(ctx)
		if err != nil {
			return err
		}
		printStatus(stdout, st)
		return nil

	case "next":
		if len(rest) > 0 {
			return apperr.New(apperr.InvalidCommand, "unexpected arguments for next")
		}
		st, err := cl.Next(ctx)
		if err != nil {
			return err
		}
		printStatus(stdout, st)
		return nil

	case "prev", "previous":
		if len(rest) > 0 {
			return apperr.New(apperr.InvalidCommand, "unexpected arguments for prev")
		}
		st, err := cl.Previous(ctx)
		if err != nil {
			return err
		}
		printStatus(stdout, st)
		return nil

	case "louder":
		if len(rest) > 0 {
			return apperr.New(apperr.InvalidCommand, "unexpected arguments for louder")
		}
		st, err := cl.VolumeUp(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Volume: %d%% (muted: %v)\n", st.Volume, st.Muted)
		return nil

	case "quieter":
		if len(rest) > 0 {
			return apperr.New(apperr.InvalidCommand, "unexpected arguments for quieter")
		}
		st, err := cl.VolumeDown(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Volume: %d%% (muted: %v)\n", st.Volume, st.Muted)
		return nil

	case "mute":
		if len(rest) > 0 {
			return apperr.New(apperr.InvalidCommand, "unexpected arguments for mute")
		}
		st, err := cl.Mute(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Muted: %v (volume: %d%%)\n", st.Muted, st.Volume)
		return nil

	case "shuffle":
		if len(rest) > 0 {
			return apperr.New(apperr.InvalidCommand, "unexpected arguments for shuffle")
		}
		st, err := cl.Shuffle(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Shuffle: %v\n", st.Shuffle)
		return nil

	case "loop":
		if len(rest) > 0 {
			return apperr.New(apperr.InvalidCommand, "unexpected arguments for loop")
		}
		st, err := cl.Loop(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Loop: %v\n", st.Loop)
		return nil

	case "playlist":
		return runPlaylistCmd(ctx, cl, p, rest, stdout)

	default:
		return apperr.New(apperr.InvalidCommand, "unknown command %q (run 'eklips help' for usage)", cmd)
	}
}

func runPlaylistCmd(ctx context.Context, cl *client.Client, p parsedFlags, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return apperr.New(apperr.InvalidCommand, "usage: eklips playlist <create|play|alias>")
	}
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "create":
		if len(rest) > 1 {
			return apperr.New(apperr.InvalidCommand, "usage: eklips playlist create [<directory>] [--output <path>] [--force]")
		}
		var dir string
		if len(rest) == 1 {
			abs, err := filepath.Abs(rest[0])
			if err != nil {
				return apperr.Wrap(apperr.InvalidCommand, err, "resolve directory")
			}
			dir = abs
		}
		out := p.output
		if out == "" && dir == "" {
			if wd, err := os.Getwd(); err == nil {
				out = filepath.Join(wd, "playlist.json")
			}
		} else if out != "" {
			if abs, err := filepath.Abs(out); err == nil {
				out = abs
			}
		}
		created, err := cl.PlaylistCreate(ctx, dir, out, p.force)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Created playlist %s\n", created)
		return nil

	case "play":
		if len(rest) != 1 {
			return apperr.New(apperr.InvalidCommand, "usage: eklips playlist play <path.json|alias> [--loop]")
		}
		target := rest[0]
		if strings.HasSuffix(strings.ToLower(target), ".json") || strings.ContainsRune(target, '/') || strings.ContainsRune(target, filepath.Separator) {
			if abs, err := filepath.Abs(target); err == nil {
				target = abs
			}
		}
		st, err := cl.PlaylistPlay(ctx, target, p.loop)
		if err != nil {
			return err
		}
		printStatus(stdout, st)
		return nil

	case "alias":
		if len(rest) == 0 {
			return apperr.New(apperr.InvalidCommand, "usage: eklips playlist alias <set|list|remove>")
		}
		action := rest[0]
		arest := rest[1:]
		switch action {
		case "set":
			if len(arest) != 2 {
				return apperr.New(apperr.InvalidCommand, "usage: eklips playlist alias set <name> <path.json>")
			}
			abs, err := filepath.Abs(arest[1])
			if err != nil {
				return apperr.Wrap(apperr.InvalidCommand, err, "resolve playlist path")
			}
			alias, err := cl.AliasSet(ctx, arest[0], abs)
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Alias %s -> %s\n", alias.Name, alias.PlaylistPath)
			return nil
		case "list":
			if len(arest) != 0 {
				return apperr.New(apperr.InvalidCommand, "usage: eklips playlist alias list")
			}
			list, err := cl.AliasList(ctx)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(stdout, "(no aliases)")
				return nil
			}
			for _, a := range list {
				fmt.Fprintf(stdout, "%s\t%s\n", a.Name, a.PlaylistPath)
			}
			return nil
		case "remove":
			if len(arest) != 1 {
				return apperr.New(apperr.InvalidCommand, "usage: eklips playlist alias remove <name>")
			}
			if err := cl.AliasRemove(ctx, arest[0]); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Removed alias %s\n", arest[0])
			return nil
		default:
			return apperr.New(apperr.InvalidCommand, "unknown alias command %q (expected set, list, or remove)", action)
		}

	default:
		return apperr.New(apperr.InvalidCommand, "unknown playlist command %q (expected create, play, or alias)", sub)
	}
}

func printStatus(w io.Writer, st engine.Status) {
	fmt.Fprintf(w, "State:        %s\n", st.State)
	fmt.Fprintf(w, "Source Type:  %s\n", st.SourceType)
	fmt.Fprintf(w, "Source Path:  %s\n", st.SourcePath)
	fmt.Fprintf(w, "Track:        %s\n", st.CurrentTrack)
	fmt.Fprintf(w, "Index:        %d\n", st.TrackIndex)
	fmt.Fprintf(w, "Position:     %d ms / %d ms\n", st.PositionMs, st.DurationMs)
	fmt.Fprintf(w, "Volume:       %d%%\n", st.Volume)
	fmt.Fprintf(w, "Muted:        %v\n", st.Muted)
	fmt.Fprintf(w, "Shuffle:      %v\n", st.Shuffle)
	fmt.Fprintf(w, "Loop:         %v\n", st.Loop)
	fmt.Fprintf(w, "Sink:         %s (listeners: %d)\n", st.Sink, st.Listeners)
}

func printQueue(w io.Writer, q engine.Queue) {
	fmt.Fprintf(w, "Source: %s (%s) — %d tracks\n", q.SourceName, q.SourceType, len(q.Tracks))
	for _, t := range q.Tracks {
		marker := " "
		if t.Current {
			marker = ">"
		}
		fmt.Fprintf(w, "%s [%d] %s\n", marker, t.Index, t.Path)
	}
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, `eklips — headless audio player service with optional terminal UI

Usage:
  eklips [serve] [options]                  Start daemon in the foreground
  eklips start [options]                    Start daemon in the background
  eklips status [options]                   Report daemon and playback status
  eklips queue [options]                    Report the loaded queue
  eklips quit [options]                     Stop the running daemon
  eklips tui [tui-options]                  Launch the built-in terminal UI
  eklips help                               Show this help

Playback commands:
  eklips play [<file|directory|.>] [--loop] Play a file/folder or resume queue
  eklips pause                              Pause playback
  eklips stop                               Stop playback (retains queue)
  eklips next                               Next track in playlist
  eklips prev                               Previous track in playlist
  eklips louder                             Increase volume by 5
  eklips quieter                            Decrease volume by 5
  eklips mute                               Toggle mute
  eklips shuffle                            Toggle shuffle
  eklips loop                               Toggle loop

Playlist commands:
  eklips playlist create [<dir>] [--output <path>] [--force]
  eklips playlist play <path.json|alias> [--loop]
  eklips playlist alias set <name> <path.json>
  eklips playlist alias list
  eklips playlist alias remove <name>

Common options:
  --db <path>          SQLite database path
  --sink <mode>        Output mode: device|http|both|null (default: device)
  --bind <address>     Control API bind address (default: 127.0.0.1)
  --port <port>        Control API port (default: 8080)
  --auth-token <tok>   Bearer token (or EKLIPS_TOKEN env)
  --allow-remote       Allow non-loopback bind without a bearer token
  --resume-on-start    Restore playback as playing on start
  --media-root <dir>   Restrict playable media to directory

TUI options:
  --remote <url>       Connect to an explicit remote daemon URL
  --no-start           Do not auto-start a local daemon if unreachable
  --stop-on-exit       Stop the daemon on TUI exit if started by this TUI
  --theme <name>       Color theme: default|nord|gruvbox|tokyonight|catppuccin
`)
}
