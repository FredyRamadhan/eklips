// Package devwatch monitors host audio output device changes (Linux
// PipeWire/PulseAudio event subscription with configurable polling fallback)
// and triggers a callback when the default output sink or its active port
// (e.g. headphones plugged/unplugged) changes.
package devwatch

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ProbeFunc returns a signature representing the active output device and port
// state (for example, "sink=alsa_output.pci|port=analog-output-headphones").
type ProbeFunc func(ctx context.Context) (string, error)

// Options configures a Watcher.
type Options struct {
	PollInterval time.Duration
	Probe        ProbeFunc
	OnChange     func(reason string)
	// DisableSubscribe forces fallback polling (used in tests).
	DisableSubscribe bool
}

// Watcher watches for audio output device changes.
type Watcher struct {
	opts   Options
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Start begins watching for output device changes in the background.
func Start(opts Options) *Watcher {
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.Probe == nil {
		opts.Probe = DefaultProbe
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &Watcher{opts: opts, cancel: cancel}
	w.wg.Add(1)
	go w.run(ctx)
	return w
}

// Stop stops the watcher and waits for its goroutine to exit.
func (w *Watcher) Stop() {
	if w == nil {
		return
	}
	w.cancel()
	w.wg.Wait()
}

func (w *Watcher) run(ctx context.Context) {
	defer w.wg.Done()

	baseline, err := w.opts.Probe(ctx)
	hasBaseline := err == nil && baseline != ""

	check := func(trigger string) {
		sig, err := w.opts.Probe(ctx)
		if err != nil || sig == "" {
			return
		}
		if !hasBaseline {
			baseline = sig
			hasBaseline = true
			return
		}
		if sig != baseline {
			prev := baseline
			baseline = sig
			if w.opts.OnChange != nil {
				w.opts.OnChange(fmt.Sprintf("output device changed (%s: %s -> %s)", trigger, prev, sig))
			}
		}
	}

	if !w.opts.DisableSubscribe {
		if pactlPath, err := exec.LookPath("pactl"); err == nil {
			if w.runSubscribe(ctx, pactlPath, check) {
				return
			}
		}
	}

	// Fallback polling loop when event subscription is unavailable.
	ticker := time.NewTicker(w.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check("poll")
		}
	}
}

func (w *Watcher) runSubscribe(ctx context.Context, pactlPath string, check func(trigger string)) bool {
	cmd := exec.CommandContext(ctx, pactlPath, "subscribe")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := sc.Text()
		if IsRelevantPactlEvent(line) {
			check("pactl-event")
		}
	}
	return ctx.Err() != nil
}

// IsRelevantPactlEvent filters `pactl subscribe` lines so only sink, server,
// or card events trigger a device-state probe. Unrelated events (sink-input,
// source, source-output, client, module, sample-cache) are ignored.
func IsRelevantPactlEvent(line string) bool {
	// Example line: Event 'change' on sink #42
	// Note: "' on sink-input" must NOT match "' on sink".
	lower := strings.ToLower(line)
	return strings.Contains(lower, " on sink #") ||
		strings.HasSuffix(lower, " on sink") ||
		strings.Contains(lower, " on server") ||
		strings.Contains(lower, " on card")
}

// DefaultProbe queries PulseAudio / PipeWire via `pactl` for the default sink
// and its active port.
func DefaultProbe(ctx context.Context) (string, error) {
	pactlPath, err := exec.LookPath("pactl")
	if err != nil {
		return "", err
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	defOut, err := exec.CommandContext(pctx, pactlPath, "get-default-sink").Output()
	if err != nil {
		return "", err
	}
	defSink := strings.TrimSpace(string(defOut))

	listOut, err := exec.CommandContext(pctx, pactlPath, "list", "sinks").Output()
	if err != nil {
		return "sink=" + defSink, nil
	}
	return ParsePactlSinks(defSink, string(listOut)), nil
}

// ParsePactlSinks extracts a deterministic signature of the default sink and
// its active output port from `pactl list sinks` output.
func ParsePactlSinks(defaultSink, sinksOutput string) string {
	var curName, activePort string
	var sinkNames []string
	var matchedPort string

	sc := bufio.NewScanner(strings.NewReader(sinksOutput))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "Name:"):
			curName = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
			sinkNames = append(sinkNames, curName)
		case strings.HasPrefix(line, "Active Port:"):
			activePort = strings.TrimSpace(strings.TrimPrefix(line, "Active Port:"))
			if curName == defaultSink || (defaultSink == "" && matchedPort == "") {
				matchedPort = activePort
			}
		}
	}
	return fmt.Sprintf("sink=%s|port=%s|count=%d", defaultSink, matchedPort, len(sinkNames))
}
