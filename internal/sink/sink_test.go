package sink

import (
	"sync"
	"testing"
	"time"

	"github.com/frdn/eklips/internal/apperr"
)

func TestBroadcasterFanOutAndSlowConsumerEviction(t *testing.T) {
	f := Format{SampleRate: 44100, Channels: 2}
	chunkBytes := 882 * 4 // 20 ms
	b := NewBroadcaster(f, chunkBytes, 100*time.Millisecond, 12)

	// Attach 10 fast listeners and 1 slow listener that never reads.
	const numFast = 10
	const numChunks = 25
	var wg sync.WaitGroup
	counts := make([]int, numFast)

	for i := 0; i < numFast; i++ {
		ch, done, cancel, err := b.Subscribe()
		if err != nil {
			t.Fatalf("Subscribe(%d): %v", i, err)
		}
		defer cancel()
		wg.Add(1)
		go func(idx int, c <-chan *Chunk, d <-chan struct{}) {
			defer wg.Done()
			for j := 0; j < numChunks; j++ {
				select {
				case chunk := <-c:
					counts[idx] += len(chunk.Data)
					chunk.Release()
				case <-d:
					return
				}
			}
		}(i, ch, done)
	}

	// Slow consumer: subscribes but never drains its channel.
	_, slowDone, slowCancel, err := b.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer slowCancel()

	// Exceeding max listeners (12) returns TOO_MANY_LISTENERS.
	_, _, extraCancel, err := b.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer extraCancel()
	if _, _, _, err := b.Subscribe(); !apperr.Is(err, apperr.TooManyListeners) {
		t.Fatalf("expected TOO_MANY_LISTENERS when exceeding cap, got %v", err)
	}

	payload := make([]byte, chunkBytes)
	for i := 0; i < numChunks; i++ {
		b.Publish(payload)
		time.Sleep(time.Millisecond)
	}

	wg.Wait()
	for i, got := range counts {
		if got != numChunks*chunkBytes {
			t.Fatalf("listener %d received %d bytes, want %d", i, got, numChunks*chunkBytes)
		}
	}

	select {
	case <-slowDone:
	case <-time.After(time.Second):
		t.Fatal("expected slow consumer to be evicted")
	}
	if b.Evictions() == 0 {
		t.Fatal("expected eviction count > 0")
	}
}

func BenchmarkBroadcasterFanOut10(b *testing.B) {
	f := Format{SampleRate: 44100, Channels: 2}
	chunkBytes := 882 * 4
	bc := NewBroadcaster(f, chunkBytes, 2*time.Second, 32)
	var chans []<-chan *Chunk

	for i := 0; i < 10; i++ {
		ch, _, cancel, err := bc.Subscribe()
		if err != nil {
			b.Fatal(err)
		}
		defer cancel()
		chans = append(chans, ch)
	}

	payload := make([]byte, chunkBytes)
	b.ResetTimer()
	b.SetBytes(int64(chunkBytes))
	for i := 0; i < b.N; i++ {
		bc.Publish(payload)
		for _, ch := range chans {
			c := <-ch
			c.Release()
		}
	}
}
