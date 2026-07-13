//go:build linux

package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// resolveDeviceName resolves a device path to the canonical kernel name used
// under /sys and /sys/kernel/debug (e.g. /dev/mapper/vg-lv -> dm-0,
// /dev/disk/by-id/... -> sda), and reports whether the device is a
// partition. Falls back to the path's basename if sysfs can't be consulted.
func resolveDeviceName(dev string) (string, bool) {
	fallback := filepath.Base(dev)
	var st unix.Stat_t
	if err := unix.Stat(dev, &st); err != nil {
		return fallback, false
	}
	if st.Mode&unix.S_IFMT != unix.S_IFBLK {
		log.Fatalf("%v is not a block device", dev)
	}
	sysPath := fmt.Sprintf("/sys/dev/block/%d:%d", unix.Major(st.Rdev), unix.Minor(st.Rdev))
	target, err := os.Readlink(sysPath)
	if err != nil {
		return fallback, false
	}
	_, partErr := os.Stat(sysPath + "/partition")
	return filepath.Base(target), partErr == nil
}

func getTotalDeviceSectorsSize(deviceBaseName string) uint64 {
	b, err := os.ReadFile(fmt.Sprintf("/sys/class/block/%s/size", deviceBaseName))
	if err != nil {
		log.Fatalf("Cannot read device block size %v", err)
		return 0
	}

	i, err := strconv.ParseUint(strings.Trim(string(b), "\r\n\t "), 10, 64)
	if err != nil {
		log.Fatalf("Cannot parse device block size %v", err)
		return 0
	}

	return i
}

func getBlkTraceDrops(deviceBaseName string) uint64 {
	b, err := os.ReadFile(fmt.Sprintf("/sys/kernel/debug/block/%s/dropped", deviceBaseName))
	if err != nil {
		log.Printf("Cannot read device drops %v", err)
		return 0
	}

	i, err := strconv.ParseUint(strings.Trim(string(b), "\r\n\t "), 10, 64)
	if err != nil {
		fatalf("Cannot parse device drops %v", err)
		return 0
	}

	return i
}

var (
	BlkTraceBufSize  = flag.Int("blktrace.bufsize", 65536, "The size of each buffer for blktrace")
	BlkTraceBufCount = flag.Int("blktrace.bufcount", 16, "The amount of buffers for blktrace to keep spare")
)

func setupBlkTrace(f *os.File, eventConsumer chan BLK_io_trace, deviceBaseName string) {
	traceOpts := BLK_user_trace_setup{
		Act_mask: 2,
		Buf_size: uint32(*BlkTraceBufSize),
		Buf_nr:   uint32(*BlkTraceBufCount),
	}

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), BLKTRACESETUP, uintptr(unsafe.Pointer(&traceOpts)))
	if errno != 0 {
		shutdownBlkTrace(f)
		log.Fatalf("failed to BLKTRACESETUP -> %v", errno)
	}

	_, _, errno = unix.Syscall(unix.SYS_IOCTL, f.Fd(), BLKTRACESTART, 0)
	if errno != 0 {
		unix.Syscall(unix.SYS_IOCTL, f.Fd(), BLKTRACETEARDOWN, 0)
		log.Fatalf("failed to BLKTRACESTART -> %v", errno)
	}
	tracedFile = f

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, unix.SIGTERM)
	go func() {
		sig := <-c
		log.Printf("Got %s signal. Aborting...", sig)
		shutdownBlkTrace(f)
		os.Exit(1)
	}()

	// One reader per relay file actually present, rather than one per
	// runtime.NumCPU(): under a CPU affinity mask or cgroup cpuset, NumCPU
	// can be smaller than the number of online CPUs, and any unread relay
	// file would silently fill up and drop events.
	traceDir := fmt.Sprintf("/sys/kernel/debug/block/%s", deviceBaseName)
	entries, err := os.ReadDir(traceDir)
	if err != nil {
		shutdownBlkTrace(f)
		log.Fatalf("cannot list %s (is debugfs mounted?) -- %v", traceDir, err)
	}
	traceFiles := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "trace") {
			continue
		}
		if _, err := strconv.Atoi(name[len("trace"):]); err != nil {
			continue
		}
		traceFiles++
		traceReaders.Add(1)
		go readBlkTraceEventFiles(traceDir+"/"+name, eventConsumer)
	}
	if traceFiles == 0 {
		shutdownBlkTrace(f)
		log.Fatalf("no per-CPU trace files found in %s", traceDir)
	}
}

var (
	traceReaders  sync.WaitGroup
	traceStopping int32
)

// stopAndDrainBlkTrace stops the kernel side of the trace and then waits for
// the reader goroutines to consume everything left in the relay buffers.
// BLKTRACETEARDOWN must not happen until this returns, or buffered events
// are lost.
func stopAndDrainBlkTrace(f *os.File) {
	unix.Syscall(unix.SYS_IOCTL, f.Fd(), BLKTRACESTOP, 0)
	atomic.StoreInt32(&traceStopping, 1)
	traceReaders.Wait()
}

func shutdownBlkTrace(f *os.File) {
	unix.Syscall(unix.SYS_IOCTL, f.Fd(), BLKTRACESTOP, 0)
	unix.Syscall(unix.SYS_IOCTL, f.Fd(), BLKTRACETEARDOWN, 0)
}

func readBlkTraceEventFiles(path string, out chan BLK_io_trace) {
	defer traceReaders.Done()
	f, err := os.Open(path)
	if err != nil {
		fatalf("Cant open trace debugfs file %s ?! %v", path, err)
	}
	defer f.Close()

	// Payload scratch space, reused across events. Len is a uint16 so 64KiB
	// always fits the largest possible payload.
	payload := make([]byte, 1<<16)

	for {
		BlkEvent := BLK_io_trace{}
		err := binary.Read(f, binary.LittleEndian, &BlkEvent)

		if err != nil {
			if err == io.EOF {
				if atomic.LoadInt32(&traceStopping) != 0 {
					return // relay buffer fully drained
				}
				time.Sleep(time.Millisecond * 10)
				continue
			}
			if atomic.LoadInt32(&traceStopping) != 0 {
				log.Printf("trace reader %s error while draining: %v", path, err)
				return
			}
			// A dead reader means untracked writes, which means a corrupt image.
			fatalf("trace reader %s failed (%v), cannot safely image device", path, err)
		}

		if BlkEvent.Magic&0xffffff00 != 0x65617400 {
			fatalf("trace reader %s desynchronised (bad magic %x), cannot safely image device", path, BlkEvent.Magic)
		}

		// The payload must be consumed in full, or the next struct read
		// decodes from the middle of this event's payload.
		data := payload[:BlkEvent.Len]
		for got := 0; got < len(data); {
			n, err := f.Read(data[got:])
			got += n
			if err != nil {
				if err == io.EOF {
					if atomic.LoadInt32(&traceStopping) != 0 {
						log.Printf("trace reader %s stopped mid-event", path)
						return
					}
					time.Sleep(time.Millisecond * 10)
					continue
				}
				if atomic.LoadInt32(&traceStopping) != 0 {
					log.Printf("trace reader %s error while draining: %v", path, err)
					return
				}
				fatalf("trace reader %s failed (%v), cannot safely image device", path, err)
			}
		}
		if BlkEvent.Error != 0 {
			if !(BlkEvent.Action&BLK_TA_UNPLUG_TIMER > 0) {
				log.Printf("Error !!!!!!!!!!!!!!!!!!!!! %#v", BlkEvent)
			}
		} else {
			// log.Printf("%#v", BlkEvent)
		}
		out <- BlkEvent
	}

}

const (
	BLK_TC_READ     = 1 << 0  /* reads */
	BLK_TC_WRITE    = 1 << 1  /* writes */
	BLK_TC_BARRIER  = 1 << 2  /* barrier */
	BLK_TC_SYNC     = 1 << 3  /* sync IO */
	BLK_TC_QUEUE    = 1 << 4  /* queueing/merging */
	BLK_TC_REQUEUE  = 1 << 5  /* requeueing */
	BLK_TC_ISSUE    = 1 << 6  /* issue */
	BLK_TC_COMPLETE = 1 << 7  /* completions */
	BLK_TC_FS       = 1 << 8  /* fs requests */
	BLK_TC_PC       = 1 << 9  /* pc requests */
	BLK_TC_NOTIFY   = 1 << 10 /* special message */
	BLK_TC_AHEAD    = 1 << 11 /* readahead */
	BLK_TC_META     = 1 << 12 /* metadata */
	BLK_TC_DISCARD  = 1 << 13 /* discard requests */
	BLK_TC_DRV_DATA = 1 << 14 /* binary per-driver data */
	BLK_TC_END      = 1 << 15 /* only 16-bits, reminder */

	BLK_TA_QUEUE        = 1 << 16 /* queued */
	BLK_TA_BACKMERGE    = 1 << 17 /* back merged to existing rq */
	BLK_TA_FRONTMERGE   = 1 << 18 /* front merge to existing rq */
	BLK_TA_GETRQ        = 1 << 19 /* allocated new request */
	BLK_TA_SLEEPRQ      = 1 << 20 /* sleeping on rq allocation */
	BLK_TA_REQUEUE      = 1 << 21 /* request requeued */
	BLK_TA_ISSUE        = 1 << 22 /* sent to driver */
	BLK_TA_COMPLETE     = 1 << 23 /* completed by driver */
	BLK_TA_PLUG         = 1 << 24 /* queue was plugged */
	BLK_TA_UNPLUG_IO    = 1 << 25 /* queue was unplugged by io */
	BLK_TA_UNPLUG_TIMER = 1 << 26 /* queue was unplugged by timer */
	BLK_TA_INSERT       = 1 << 27 /* insert request */
	BLK_TA_SPLIT        = 1 << 28 /* bio was split */
	BLK_TA_BOUNCE       = 1 << 29 /* bio was bounced */
	BLK_TA_REMAP        = 1 << 30 /* bio was remapped */
	BLK_TA_ABORT        = 1 << 31 /* request aborted */
	BLK_TA_DRV_DATA     = 1 << 32 /* driver-specific binary data */
)

func unpackBits(in uint32) string {
	in2 := uint64(in)
	o := ""

	if in2&(1<<0) > 0 {
		o += " BLK_TC_READ"
	}
	if in2&(1<<1) > 0 {
		o += " BLK_TC_WRITE"
	}
	if in2&(1<<2) > 0 {
		o += " BLK_TC_BARRIER"
	}
	if in2&(1<<3) > 0 {
		o += " BLK_TC_SYNC"
	}
	if in2&(1<<4) > 0 {
		o += " BLK_TC_QUEUE"
	}
	if in2&(1<<5) > 0 {
		o += " BLK_TC_REQUEUE"
	}
	if in2&(1<<6) > 0 {
		o += " BLK_TC_ISSUE"
	}
	if in2&(1<<7) > 0 {
		o += " BLK_TC_COMPLETE"
	}
	if in2&(1<<8) > 0 {
		o += " BLK_TC_FS"
	}
	if in2&(1<<9) > 0 {
		o += " BLK_TC_PC"
	}
	if in2&(1<<10) > 0 {
		o += " BLK_TC_NOTIFY"
	}
	if in2&(1<<11) > 0 {
		o += " BLK_TC_AHEAD"
	}
	if in2&(1<<12) > 0 {
		o += " BLK_TC_META"
	}
	if in2&(1<<13) > 0 {
		o += " BLK_TC_DISCARD"
	}
	if in2&(1<<14) > 0 {
		o += " BLK_TC_DRV_DATA"
	}
	if in2&(1<<15) > 0 {
		o += " BLK_TC_END"
	}
	if in2&(1<<16) > 0 {
		o += " BLK_TA_QUEUE"
	}
	if in2&(1<<17) > 0 {
		o += " BLK_TA_BACKMERGE"
	}
	if in2&(1<<18) > 0 {
		o += " BLK_TA_FRONTMERGE"
	}
	if in2&(1<<19) > 0 {
		o += " BLK_TA_GETRQ"
	}
	if in2&(1<<20) > 0 {
		o += " BLK_TA_SLEEPRQ"
	}
	if in2&(1<<21) > 0 {
		o += " BLK_TA_REQUEUE"
	}
	if in2&(1<<22) > 0 {
		o += " BLK_TA_ISSUE"
	}
	if in2&(1<<23) > 0 {
		o += " BLK_TA_COMPLETE"
	}
	if in2&(1<<24) > 0 {
		o += " BLK_TA_PLUG"
	}
	if in2&(1<<25) > 0 {
		o += " BLK_TA_UNPLUG_IO"
	}
	if in2&(1<<26) > 0 {
		o += " BLK_TA_UNPLUG_TIMER"
	}
	if in2&(1<<27) > 0 {
		o += " BLK_TA_INSERT"
	}
	if in2&(1<<28) > 0 {
		o += " BLK_TA_SPLIT"
	}
	if in2&(1<<29) > 0 {
		o += " BLK_TA_BOUNCE"
	}
	if in2&(1<<30) > 0 {
		o += " BLK_TA_REMAP"
	}
	if in2&(1<<31) > 0 {
		o += " BLK_TA_ABORT"
	}

	return o
}
