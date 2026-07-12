package main

import (
	"log"
	"math/bits"
	"sync/atomic"
)

// This will need 270MB~ per TB of disk being tracked
type DirtySectorTracker struct {
	TotalSizeOfDevice uint64
	Sectors           uint64
	dirtyTracker      []uint64
	// DirtySectors: CountDirty() must be called for this number to be updated
	DirtySectors     int
	warnedOutOfRange bool
}

func (d *DirtySectorTracker) Setup(diskSize uint64) {
	d.dirtyTracker = make([]uint64, ((diskSize)/64)+2)
}

func (d *DirtySectorTracker) SetDirty(sector uint64) {
	arrayTarget := sector / 64
	if arrayTarget >= uint64(len(d.dirtyTracker)) {
		// Writes beyond the reported device size (the overread region) have no
		// slot in the bitmap and must not crash the imaging run.
		if !d.warnedOutOfRange {
			d.warnedOutOfRange = true
			log.Printf("Write event beyond device end (sector %d) cannot be tracked", sector)
		}
		return
	}
	mask := uint64(1) << (sector % 64)
	for {
		old := atomic.LoadUint64(&d.dirtyTracker[arrayTarget])
		if old&mask != 0 {
			return
		}
		if atomic.CompareAndSwapUint64(&d.dirtyTracker[arrayTarget], old, old|mask) {
			return
		}
	}
}

func (d *DirtySectorTracker) CountDirty() {
	dirty := 0
	for i := 0; i < len(d.dirtyTracker); i++ {
		v := atomic.LoadUint64(&d.dirtyTracker[i])
		if v != 0 {
			dirty += bits.OnesCount64(v)
		}
	}

	d.DirtySectors = dirty
}

// GetDirtySectors Gives a full list of sectors (in order) that have been marked as dirty
func (d *DirtySectorTracker) GetDirtySectors() chan uint64 {
	o := make(chan uint64)
	go func() {
		for i := 0; i < len(d.dirtyTracker); i++ {
			v := atomic.LoadUint64(&d.dirtyTracker[i])
			if v != 0 {
				for b := 0; b < 64; b++ {
					if v&(1<<b) > 0 {
						o <- uint64((i * 64) + b)
					}
				}
			}
		}
		close(o)
	}()

	return o
}
