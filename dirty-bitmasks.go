package main

import (
	"log"
	"math/bits"
	"sync/atomic"
)

// DirtySectorTracker is a bitmap with one bit per 512-byte sector. It is
// safe for concurrent use: the trace readers mark sectors while the
// catch-up pass takes them. This will need 256MB~ per TB of disk being
// tracked.
type DirtySectorTracker struct {
	sectors      uint64
	dirtyTracker []uint64
	// dirtyCount tracks how many bits are set, maintained as bits are set
	// and taken, so reading it doesn't need a scan of the whole bitmap
	// (which is gigabytes of memory traffic per poll on multi-TB devices).
	dirtyCount       int64
	warnedOutOfRange int32
}

func (d *DirtySectorTracker) Setup(sectors uint64) {
	d.sectors = sectors
	d.dirtyTracker = make([]uint64, (sectors+63)/64)
}

func (d *DirtySectorTracker) SetDirty(sector uint64) {
	d.SetRange(sector, 1)
}

// SetRange marks sectors [start, start+count) dirty, a whole bitmap word at
// a time. Sectors past the end of the device have no slot in the bitmap
// and are ignored, with a one-time warning, rather than crashing the
// imaging run.
func (d *DirtySectorTracker) SetRange(start, count uint64) {
	end := start + count
	if end < start {
		end = ^uint64(0)
	}
	if end > d.sectors {
		if atomic.CompareAndSwapInt32(&d.warnedOutOfRange, 0, 1) {
			log.Printf("Write event beyond device end (sectors %d-%d) cannot be tracked", start, end-1)
		}
		end = d.sectors
	}
	for start < end {
		bit := start % 64
		n := 64 - bit
		if end-start < n {
			n = end - start
		}
		d.setBits(start/64, ^uint64(0)>>(64-n)<<bit)
		start += n
	}
}

func (d *DirtySectorTracker) setBits(word, mask uint64) {
	p := &d.dirtyTracker[word]
	for {
		old := atomic.LoadUint64(p)
		if old&mask == mask {
			return
		}
		if atomic.CompareAndSwapUint64(p, old, old|mask) {
			atomic.AddInt64(&d.dirtyCount, int64(bits.OnesCount64(mask&^old)))
			return
		}
	}
}

// Count returns how many sectors are currently marked dirty.
func (d *DirtySectorTracker) Count() int64 {
	if n := atomic.LoadInt64(&d.dirtyCount); n > 0 {
		return n
	}
	// A take can briefly run ahead of the increment for a bit it took.
	return 0
}

// countByScan recounts from the bitmap itself; kept for tests to validate
// the incremental counter against.
func (d *DirtySectorTracker) countByScan() int64 {
	dirty := int64(0)
	for i := 0; i < len(d.dirtyTracker); i++ {
		v := atomic.LoadUint64(&d.dirtyTracker[i])
		if v != 0 {
			dirty += int64(bits.OnesCount64(v))
		}
	}
	return dirty
}

// TakeRuns sweeps the bitmap in ascending sector order, atomically clearing
// each word as it goes, and calls emit for every run of consecutive dirty
// sectors (at most maxRun sectors each). A sector marked again after its
// word was swept stays marked for the next sweep, so concurrent marking is
// never lost. It stops at the first error from emit and returns the number
// of sectors taken.
func (d *DirtySectorTracker) TakeRuns(maxRun uint64, emit func(start, count uint64) error) (int64, error) {
	var taken int64
	var runStart, runLen uint64
	flush := func() error {
		if runLen == 0 {
			return nil
		}
		err := emit(runStart, runLen)
		runLen = 0
		return err
	}
	for w := range d.dirtyTracker {
		p := &d.dirtyTracker[w]
		if atomic.LoadUint64(p) == 0 {
			if err := flush(); err != nil {
				return taken, err
			}
			continue
		}
		v := atomic.SwapUint64(p, 0)
		n := int64(bits.OnesCount64(v))
		atomic.AddInt64(&d.dirtyCount, -n)
		taken += n
		for b := uint64(0); b < 64; b++ {
			if v&(1<<b) == 0 {
				if err := flush(); err != nil {
					return taken, err
				}
				continue
			}
			sector := uint64(w)*64 + b
			if runLen > 0 && runStart+runLen == sector && runLen < maxRun {
				runLen++
				continue
			}
			if err := flush(); err != nil {
				return taken, err
			}
			runStart, runLen = sector, 1
		}
	}
	return taken, flush()
}
