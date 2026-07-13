package main

import (
	"log"
	"sync"
	"testing"
)

func TestDirtyBitMasks(t *testing.T) {
	Round1 := DirtySectorTracker{}
	Round1.Setup(1e7)
	for i := 0; i < 512; i++ {
		Round1.SetDirty(uint64(i))
	}

	set := make(map[uint64]bool)
	a := Round1.GetDirtySectors()
	for v := range a {
		set[v] = true
	}

	for i := 0; i < 512; i++ {
		if set[uint64(i)] == false {
			log.Printf("Sector %d should have been set as dirty", i)
			t.Fail()
		}
	}

}

func TestDirtyBitMasks2(t *testing.T) {
	Round1 := DirtySectorTracker{}
	Round1.Setup(1e7)
	for i := 0; i < 1024; i++ {
		if i%27 == 0 {

		} else {
			Round1.SetDirty(uint64(i))
		}
	}

	set := make(map[uint64]bool)
	a := Round1.GetDirtySectors()
	for v := range a {
		set[v] = true
	}

	for i := 0; i < 512; i++ {
		if set[uint64(i)] == (i%27 == 0) {
			log.Printf("Sector %d should not have been set", i)
			t.Fail()
		}
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
