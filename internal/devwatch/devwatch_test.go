package devwatch

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsRelevantPactlEvent(t *testing.T) {
	relevant := []string{
		"Event 'change' on sink #0",
		"Event 'new' on sink #2",
		"Event 'remove' on sink #1",
		"Event 'change' on server #0",
		"Event 'change' on card #0",
	}
	for _, line := range relevant {
		if !IsRelevantPactlEvent(line) {
			t.Errorf("expected %q to be relevant", line)
		}
	}

	unrelated := []string{
		"Event 'change' on sink-input #45",
		"Event 'new' on client #102",
		"Event 'change' on source #1",
		"Event 'change' on source-output #9",
	}
	for _, line := range unrelated {
		if IsRelevantPactlEvent(line) {
			t.Errorf("expected %q to be ignored as unrelated", line)
		}
	}
}

func TestParsePactlSinks(t *testing.T) {
	sample := `Sink #52
	State: RUNNING
	Name: alsa_output.pci-0000_00_1f.3.analog-stereo
	Description: Built-in Audio Analog Stereo
	Volume: front-left: 45000 /  69%
	Active Port: analog-output-headphones
`
	sig := ParsePactlSinks("alsa_output.pci-0000_00_1f.3.analog-stereo", sample)
	want := "sink=alsa_output.pci-0000_00_1f.3.analog-stereo|port=analog-output-headphones|count=1"
	if sig != want {
		t.Fatalf("ParsePactlSinks = %q, want %q", sig, want)
	}
}

func TestWatcherPausesOnlyOnActualOutputDeviceChange(t *testing.T) {
	var currentSig atomic.Value
	currentSig.Store("sink=speakers|port=analog-output-speaker|count=1")

	var changes atomic.Int32
	w := Start(Options{
		PollInterval:     15 * time.Millisecond,
		DisableSubscribe: true,
		Probe: func(context.Context) (string, error) {
			return currentSig.Load().(string), nil
		},
		OnChange: func(string) {
			changes.Add(1)
		},
	})
	defer w.Stop()

	// Unrelated polls with identical signature must not trigger OnChange.
	time.Sleep(50 * time.Millisecond)
	if got := changes.Load(); got != 0 {
		t.Fatalf("expected 0 changes when signature is unchanged, got %d", got)
	}

	// Simulate plugging in headphones (active port changes).
	currentSig.Store("sink=speakers|port=analog-output-headphones|count=1")
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && changes.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := changes.Load(); got != 1 {
		t.Fatalf("expected 1 device change event after headphone plug, got %d", got)
	}
}
