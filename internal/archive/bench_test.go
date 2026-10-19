package archive

import (
	"path/filepath"
	"testing"
	"time"

	"reproxy/internal/capture"
)

// BenchmarkRecord measures what the archive costs the proxy: Record is called
// on the goroutine relaying the request, so this is the number that has to
// stay small. The IDs cycle over a window the writer can keep up with, which
// is the steady state — the queue holds one flush interval of traffic, not
// the whole session.
func BenchmarkRecord(b *testing.B) {
	a, err := Open(filepath.Join(b.TempDir(), FileName))
	if err != nil {
		b.Fatal(err)
	}
	defer a.Close()

	const window = 512
	f := sample(1)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		f.ID = uint64(i % window)
		a.Record(capture.Event{Kind: capture.FlowAdded, Flow: f})
	}
}

// BenchmarkWriteThroughput measures flows written to disk per second,
// including the commit.
func BenchmarkWriteThroughput(b *testing.B) {
	a, err := Open(filepath.Join(b.TempDir(), FileName))
	if err != nil {
		b.Fatal(err)
	}
	defer a.Close()

	f := sample(1)
	for i := 0; b.Loop(); i++ {
		f.ID = uint64(i)
		a.Record(capture.Event{Kind: capture.FlowAdded, Flow: f})
	}
	if err := a.Flush(); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkRecent measures reopening an engagement: how long it takes to pull
// saved traffic back into a store.
func BenchmarkRecent(b *testing.B) {
	a, err := Open(filepath.Join(b.TempDir(), FileName))
	if err != nil {
		b.Fatal(err)
	}
	defer a.Close()

	const flows = 5000
	f := sample(1)
	f.Started = time.Now()
	for i := 1; i <= flows; i++ {
		f.ID = uint64(i)
		a.Record(capture.Event{Kind: capture.FlowAdded, Flow: f})
	}
	if err := a.Flush(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for b.Loop() {
		got, err := a.Recent(LoadOptions{})
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != flows {
			b.Fatalf("loaded %d flows", len(got))
		}
	}
}

// BenchmarkRecentMetadata is the same load with the bodies left behind, which
// is what the request list alone needs.
func BenchmarkRecentMetadata(b *testing.B) {
	a, err := Open(filepath.Join(b.TempDir(), FileName))
	if err != nil {
		b.Fatal(err)
	}
	defer a.Close()

	const flows = 5000
	f := sample(1)
	f.Started = time.Now()
	for i := 1; i <= flows; i++ {
		f.ID = uint64(i)
		a.Record(capture.Event{Kind: capture.FlowAdded, Flow: f})
	}
	if err := a.Flush(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for b.Loop() {
		got, err := a.Recent(LoadOptions{MaxBytes: 1})
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != flows {
			b.Fatalf("loaded %d flows", len(got))
		}
	}
}
