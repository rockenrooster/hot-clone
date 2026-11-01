package main

import (
	"sync"
	"testing"
)

// takeAll drains the tracker and returns the set of sectors it held.
func takeAll(t *testing.T, d *DirtySectorTracker) map[uint64]bool {
	t.Helper()
	set := make(map[uint64]bool)
	_, err := d.TakeRuns(maxRunSectors, func(start, count uint64) error {
		for s := start; s < start+count; s++ {
			if set[s] {
				t.Errorf("sector %d emitted twice", s)
			}
			set[s] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func TestDirtyBitMasks(t *testing.T) {
	Round1 := DirtySectorTracker{}
	Round1.Setup(1e7)
	for i := 0; i < 512; i++ {
		Round1.SetDirty(uint64(i))
	}

	set := takeAll(t, &Round1)
	for i := 0; i < 512; i++ {
		if !set[uint64(i)] {
			t.Errorf("Sector %d should have been set as dirty", i)
		}
	}
	if len(set) != 512 {
		t.Errorf("got %d dirty sectors, want 512", len(set))
	}
}

func TestDirtyBitMasks2(t *testing.T) {
	Round1 := DirtySectorTracker{}
	Round1.Setup(1e7)
	for i := 0; i < 1024; i++ {
		if i%27 != 0 {
			Round1.SetDirty(uint64(i))
		}
	}

	set := takeAll(t, &Round1)
	for i := 0; i < 1024; i++ {
		if set[uint64(i)] == (i%27 == 0) {
			t.Errorf("Sector %d: dirty=%v, want %v", i, set[uint64(i)], i%27 != 0)
		}
	}
}

// SetRange must set exactly [start, start+count) across word boundaries,
// and clip at the end of the device.
func TestSetRange(t *testing.T) {
	cases := []struct{ start, count uint64 }{
		{0, 1}, {63, 1}, {63, 2}, {0, 64}, {1, 64}, {5, 200}, {128, 64}, {990, 50},
	}
	for _, c := range cases {
		d := DirtySectorTracker{}
		d.Setup(1000)
		d.SetRange(c.start, c.count)
		want := c.count
		if c.start+c.count > 1000 {
			want = 1000 - c.start
		}
		if got := d.Count(); got != int64(want) {
			t.Errorf("SetRange(%d, %d): count %d, want %d", c.start, c.count, got, want)
		}
		set := takeAll(t, &d)
		for s := uint64(0); s < 1000; s++ {
			in := s >= c.start && s < c.start+c.count
			if set[s] != in {
				t.Errorf("SetRange(%d, %d): sector %d dirty=%v, want %v", c.start, c.count, s, set[s], in)
			}
		}
		if d.Count() != 0 || d.countByScan() != 0 {
			t.Errorf("SetRange(%d, %d): tracker not empty after taking everything", c.start, c.count)
		}
	}
}

// TakeRuns must merge consecutive sectors, split at gaps and word ends
// correctly, and cap runs at maxRun.
func TestTakeRunsCoalesces(t *testing.T) {
	d := DirtySectorTracker{}
	d.Setup(10000)
	d.SetRange(10, 5)    // run inside one word
	d.SetRange(60, 10)   // run crossing a word boundary
	d.SetRange(200, 300) // longer than maxRun below
	d.SetDirty(9999)     // last sector of the device

	type run struct{ start, count uint64 }
	var got []run
	taken, err := d.TakeRuns(128, func(start, count uint64) error {
		got = append(got, run{start, count})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []run{{10, 5}, {60, 10}, {200, 128}, {328, 128}, {456, 44}, {9999, 1}}
	if len(got) != len(want) {
		t.Fatalf("runs %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("runs %v, want %v", got, want)
		}
	}
	if taken != 5+10+300+1 {
		t.Errorf("taken %d, want %d", taken, 5+10+300+1)
	}
}

// TestDirtyCounterMatchesBitmap hammers SetDirty from several goroutines
// with overlapping sectors and checks the incremental counter agrees with a
// scan of the bitmap itself. Run under -race this also validates the CAS
// and counter ordering.
func TestDirtyCounterMatchesBitmap(t *testing.T) {
	d := DirtySectorTracker{}
	d.Setup(1 << 20)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20000; i++ {
				d.SetDirty(uint64((i * (g + 1)) % 100000))
			}
		}(g)
	}
	wg.Wait()

	if d.Count() != d.countByScan() {
		t.Fatalf("incremental count %d != bitmap scan %d", d.Count(), d.countByScan())
	}
	if d.Count() == 0 {
		t.Fatal("counter never advanced")
	}
}

// Sectors marked while a sweep is taking them must either be taken by that
// sweep or still be marked afterwards -- never lost. That is what lets the
// catch-up rounds run with tracing still on.
func TestTakeRunsConcurrentMarking(t *testing.T) {
	const sectors = 1 << 16
	d := DirtySectorTracker{}
	d.Setup(sectors)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := uint64(0); i < sectors; i++ {
			d.SetDirty((i * 7919) % sectors) // every sector once, scattered
		}
	}()

	seen := make([]bool, sectors)
	take := func() {
		if _, err := d.TakeRuns(maxRunSectors, func(start, count uint64) error {
			for s := start; s < start+count; s++ {
				seen[s] = true
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		take()
	}
	take() // after marking stopped, like the final catch-up round

	for s := range seen {
		if !seen[s] {
			t.Fatalf("sector %d was marked but never taken", s)
		}
	}
	if d.Count() != 0 || d.countByScan() != 0 {
		t.Fatalf("tracker not empty after final sweep: count %d, scan %d", d.Count(), d.countByScan())
	}
}
