package main

import (
	"fmt"
	"io"
	"unsafe"
)

// maxRunSectors caps one catch-up section at 1 MiB.
const maxRunSectors = 2048

// alignedBuffer returns a size-byte slice whose start is aligned to align
// bytes (a power of two), as O_DIRECT reads need.
func alignedBuffer(size, align int) []byte {
	buf := make([]byte, size+align)
	off := int(uintptr(unsafe.Pointer(&buf[0])) & uintptr(align-1))
	if off != 0 {
		off = align - off
	}
	return buf[off : off+size : off+size]
}

// alignRun widens the sector run [start, start+count) to whole logical
// blocks of lbsSectors sectors, clipped to the end of the device.
func alignRun(start, count, lbsSectors, deviceSectors uint64) (first, end uint64) {
	first = start / lbsSectors * lbsSectors
	end = (start + count + lbsSectors - 1) / lbsSectors * lbsSectors
	if end > deviceSectors {
		end = deviceSectors
	}
	return first, end
}

// copyDirty takes every sector currently marked dirty, clearing it so that
// writes landing from now on are marked afresh for the next round, and
// appends a new copy of each run of them to the image as one section.
// Runs are widened to whole logical blocks because the device is read with
// O_DIRECT; that only re-reads a few extra sectors, which is harmless. buf
// must hold maxRunSectors plus two logical blocks. Returns the number of
// dirty sectors taken.
func copyDirty(dev io.ReaderAt, out io.Writer, d *DirtySectorTracker, lbsSectors, deviceSectors uint64, buf []byte) (int64, error) {
	return d.TakeRuns(maxRunSectors, func(start, count uint64) error {
		first, end := alignRun(start, count, lbsSectors, deviceSectors)
		if end <= first {
			return nil
		}
		chunk := buf[:(end-first)*512]
		if _, err := dev.ReadAt(chunk, int64(first*512)); err != nil {
			return fmt.Errorf("Read for catchup failed (sectors %d-%d) -- %v", first, end-1, err)
		}
		if _, err := fmt.Fprintf(out, "S:%d\tL:%d\n", first, len(chunk)); err != nil {
			return fmt.Errorf("Output file/device write failure -- %v", err)
		}
		if _, err := out.Write(chunk); err != nil {
			return fmt.Errorf("Output file/device write failure -- %v", err)
		}
		return nil
	})
}
