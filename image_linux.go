//go:build linux

package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

var DST DirtySectorTracker

var (
	dumpWrites     = flag.Bool("print-writes", false, "print all writes happening")
	keepCache      = flag.Bool("keep-cache", false, "keep the device's pages in the page cache while reading (default is to drop them behind the read to limit impact on the live system)")
	allowPartition = flag.Bool("allow-partition", false, "allow imaging a partition device (UNSAFE: blktrace reports absolute disk sectors and partition handling is unverified, concurrent writes may be tracked at the wrong offsets)")
)

var bytesRead int64

// tracedFile is set once blktrace is enabled on the device. log.Fatalf exits
// without running defers, so fatal paths must tear the trace down themselves.
var tracedFile *os.File

func fatalf(format string, v ...interface{}) {
	if tracedFile != nil {
		shutdownBlkTrace(tracedFile)
	}
	log.Fatalf(format, v...)
}

func imageMain() {
	deviceBaseName, isPartition := resolveDeviceName(*dev)
	if isPartition && !*allowPartition {
		log.Fatalf("%v is a partition. blktrace reports absolute disk sectors and hot-clone's partition handling is unverified, so writes landing during the copy could be tracked at the wrong offsets and leave silently stale data in the image. Image the whole disk instead, or pass -allow-partition to accept the risk.", *dev)
	}

	eventConsumer := make(chan BLK_io_trace, 65536)

	f, err := os.Open(*dev)
	if err != nil {
		log.Fatalf("cannot open device %v - %v", *dev, err)
	}

	diskSectorsCount := getTotalDeviceSectorsSize(deviceBaseName)
	setupBlkTrace(f, eventConsumer, deviceBaseName)
	defer shutdownBlkTrace(f)

	DST = DirtySectorTracker{}
	DST.Setup(diskSectorsCount)

	// Periodic progress/safety reporter. It must be stopped before blktrace
	// teardown removes the dropped-events file it polls.
	totalBytes := int64(diskSectorsCount * 512)
	statusDone := make(chan struct{})
	statusStopped := make(chan struct{})
	go func() {
		defer close(statusStopped)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		lastBytes := int64(0)
		for {
			select {
			case <-statusDone:
				return
			case <-ticker.C:
			}
			TotalRead := atomic.LoadInt64(&bytesRead)
			rate := TotalRead - lastBytes
			lastBytes = TotalRead
			eta := "?"
			if rate > 0 && TotalRead <= totalBytes {
				eta = (time.Duration((totalBytes-TotalRead)/rate) * time.Second).String()
			}
			eventDrops := getBlkTraceDrops(deviceBaseName)
			log.Printf("Read %s of %s (%s/s, ETA %s) -- %v dirty sectors (%d event drops)",
				ByteCountIEC(TotalRead), ByteCountIEC(totalBytes), ByteCountIEC(rate), eta, DST.Count(), eventDrops)
			if eventDrops != 0 {
				fatalf("Event drops detected (%d), cannot safely image device anymore", eventDrops)
			}
		}
	}()

	trackerDone := make(chan struct{})
	go trackEvents(eventConsumer, trackerDone)

	// Begin reading the block device
	BlockF, err := os.Open(*dev)
	if err != nil {
		fatalf("cannot open block device %v - %v", *dev, err)
	}

	// Everything written through cw is covered by the CRC/byte count that
	// the end trailer records.
	cw := newCountingCRCWriter(os.Stdout)
	if _, err := fmt.Fprintf(cw, "This-Is-A-Hot-Clone-Image See: https://github.com/benjojo/hot-clone\n"); err != nil {
		fatalf("Output file/device write failure -- %v", err)
	}
	if _, err := fmt.Fprintf(cw, "S:0\tL:%d\n", diskSectorsCount*512); err != nil {
		fatalf("Output file/device write failure -- %v", err)
	}
	TotalRead := int64(0) // for use below only!!!
	BytesLeftToRead := int64(diskSectorsCount * 512)
	buf := make([]byte, 1024*1024)
	for BytesLeftToRead > 0 {
		expectedRead := int64(1024 * 1024)
		if BytesLeftToRead < expectedRead {
			expectedRead = BytesLeftToRead
		}

		chunk := buf[:expectedRead]
		n, err := io.ReadFull(BlockF, chunk)
		if err != nil {
			fatalf("Disk read failure -- %v (read %d of %d bytes, had %d bytes left)", err, n, expectedRead, BytesLeftToRead)
		}
		readStart := TotalRead
		TotalRead += int64(n)
		atomic.StoreInt64(&bytesRead, TotalRead)
		BytesLeftToRead -= int64(n)

		_, err = cw.Write(chunk)
		if err != nil {
			fatalf("Output file/device write failure -- %v", err)
		}
		if !*keepCache {
			// Advisory only: drop the pages this pass just pulled in so a
			// whole-device read doesn't evict the live system's page cache.
			unix.Fadvise(int(BlockF.Fd()), readStart, int64(n), unix.FADV_DONTNEED)
		}
	}

	alreadyWarnedAboutOverread := false
	tmpSectors := diskSectorsCount
	overreadBuf := make([]byte, 512)
	// Attempt to over-read, in case the block device is actually bigger
	for {
		n, err := BlockF.Read(overreadBuf)
		TotalRead += int64(n)
		atomic.StoreInt64(&bytesRead, TotalRead)
		if err == io.EOF {
			// we are done! time to image the other bits
			break
		} else if err != nil {
			break
		}

		if (uint64(TotalRead) > diskSectorsCount*512) && !alreadyWarnedAboutOverread {
			// Okay very interesting, the block layer let us read more data than there were sectors!
			alreadyWarnedAboutOverread = true
			log.Printf("Strange device! Lets us read more data than there are sectors!!!")
		}
		_, err = fmt.Fprintf(cw, "S:%d\tL:%d\n", tmpSectors, n)
		if err != nil {
			fatalf("Output file/device write failure -- %v", err)
		}
		_, err = cw.Write(overreadBuf[:n])
		if err != nil {
			fatalf("Output file/device write failure -- %v", err)
		}
		tmpSectors++
		if n != 512 {
			// we are now out of alignment, time to leave
			log.Printf("*And* the strange device gave us a shorter than sector read()?!")
			break
		}
	}

	if alreadyWarnedAboutOverread {
		log.Printf("Device overread by %d sectors", tmpSectors-diskSectorsCount)
	}

	// Stop tracing, then wait for every event still sitting in the kernel relay
	// buffers and the channel to be applied to the bitmap. Snapshotting the
	// dirty set with events still in flight would lose writes.
	stopAndDrainBlkTrace(f)
	close(eventConsumer)
	<-trackerDone

	// Final drop check: the counter file disappears at teardown, and the
	// periodic check may not run again before we finish.
	if drops := getBlkTraceDrops(deviceBaseName); drops != 0 {
		fatalf("Event drops detected (%d), cannot safely image device", drops)
	}
	close(statusDone)
	<-statusStopped
	shutdownBlkTrace(f)

	// now let's catch up
	totalDirty := DST.Count()
	progressStep := totalDirty / 10
	if progressStep < 1 {
		progressStep = 1
	}
	out := bufio.NewWriterSize(cw, 1024*1024)
	sectorBuf := make([]byte, 512)
	dirtySectorChannel := DST.GetDirtySectors()
	n := int64(0)
	for sector := range dirtySectorChannel {
		_, err := BlockF.Seek(int64(sector)*512, 0)
		if err != nil {
			fatalf("Seek failure (to sector %d) for catchup -- %v", sector, err)
		}
		_, err = io.ReadFull(BlockF, sectorBuf)
		if err != nil {
			fatalf("Read for catchup failed (sector %d) -- %v", sector, err)
		}
		fmt.Fprintf(out, "S:%d\tL:%d\n", sector, 512)
		_, err = out.Write(sectorBuf)
		if err != nil {
			fatalf("Output file/device write failure -- %v", err)
		}
		n++
		if n%progressStep == 0 {
			log.Printf("Catching up %d/%d sectors", n, totalDirty)
		}
	}
	err = out.Flush()
	if err != nil {
		fatalf("Output file/device write failure -- %v", err)
	}

	// The trailer itself is written outside cw: its CRC/BYTES describe
	// everything before it.
	if _, err := os.Stdout.WriteString(formatEndTrailer(cw.Sum32(), cw.Bytes())); err != nil {
		fatalf("Output file/device write failure -- %v", err)
	}
	log.Printf("Done")
}

func trackEvents(eventConsumer chan BLK_io_trace, done chan struct{}) {
	defer close(done)
	for event := range eventConsumer {
		if event.Action&(1<<BLK_TC_WRITE) > 0 {
			if *dumpWrites {
				log.Printf("Write: Sector %#v (%d) (%d bytes) | F: %x (%s)", event.Sector, event.Sector, event.Bytes, event.Action, unpackBits(event.Action))
			}
			if event.Sector == 0 && event.Bytes == 0 {
				continue
			}
			// Every write is marked dirty regardless of how far the sequential
			// read has progressed: a queued write can complete after the read
			// passes it, so whether the read observed it cannot be known.
			DST.SetDirty(event.Sector)
			otherSectors := uint64(event.Bytes) / 512
			for i := uint64(1); i < otherSectors; i++ {
				DST.SetDirty(event.Sector + i)
			}
		} else {
			if *dumpWrites {
				log.Printf("????: Sector %#v (%d) (%d bytes) | F: %x (%s)", event.Sector, event.Sector, event.Bytes, event.Action, unpackBits(event.Action))
			}
		}
	}
}
