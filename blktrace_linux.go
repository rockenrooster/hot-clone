//go:build linux

package main

import (
	"bufio"
	"flag"
	"fmt"
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

// blockDevice is the device being imaged, resolved through sysfs.
type blockDevice struct {
	path         string
	name         string // kernel name, as used under /sys/class/block and debugfs
	isPartition  bool
	major, minor uint32
}

// resolveDevice resolves a device path to the canonical kernel name used
// under /sys and /sys/kernel/debug (e.g. /dev/mapper/vg-lv -> dm-0,
// /dev/disk/by-id/... -> sda), and reports whether it is a partition.
func resolveDevice(path string) blockDevice {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		log.Fatalf("cannot stat device %v - %v", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFBLK {
		log.Fatalf("%v is not a block device", path)
	}
	d := blockDevice{
		path:  path,
		name:  filepath.Base(path),
		major: unix.Major(uint64(st.Rdev)),
		minor: unix.Minor(uint64(st.Rdev)),
	}
	sysPath := fmt.Sprintf("/sys/dev/block/%d:%d", d.major, d.minor)
	if target, err := os.Readlink(sysPath); err == nil {
		d.name = filepath.Base(target)
		_, partErr := os.Stat(sysPath + "/partition")
		d.isPartition = partErr == nil
	}
	return d
}

// deviceLock holds the per-device lock file open, and so locked, for the
// life of the process.
var deviceLock *os.File

// lockDevice makes sure only one hot-clone works on a device at a time, so
// -reset-trace can never tear down the trace of a run in progress. It is an
// flock on a file named after the device number, so every path to the
// device maps to the same lock, and the kernel releases it however the
// process exits.
func lockDevice(d blockDevice) {
	dir := "/run/lock"
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		dir = os.TempDir()
	}
	path := filepath.Join(dir, fmt.Sprintf("hot-clone-%d-%d.lock", d.major, d.minor))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0600)
	if err != nil {
		log.Fatalf("cannot open lock file %v - %v", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if err == unix.EWOULDBLOCK {
			log.Fatalf("another hot-clone is already running on %v", d.path)
		}
		log.Fatalf("cannot lock %v - %v", path, err)
	}
	deviceLock = f
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

// getBlkTraceDrops returns the kernel's count of events dropped because a
// relay buffer was full.
func getBlkTraceDrops(deviceBaseName string) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/sys/kernel/debug/block/%s/dropped", deviceBaseName))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// checkBlkTraceDrops aborts the image if any trace event was dropped, or if
// the counter can't be read: it disappears when the trace is torn down, so
// an unreadable counter means writes are no longer being tracked.
func checkBlkTraceDrops(deviceBaseName string) {
	drops, err := getBlkTraceDrops(deviceBaseName)
	if err != nil {
		fatalf("Cannot read blktrace event drop counter (%v), the trace may have been removed, cannot safely image device", err)
	}
	if drops != 0 {
		fatalf("Event drops detected (%d), cannot safely image device", drops)
	}
}

var (
	BlkTraceBufSize  = flag.Int("blktrace.bufsize", 65536, "The size of each buffer for blktrace")
	BlkTraceBufCount = flag.Int("blktrace.bufcount", 16, "The amount of buffers for blktrace to keep spare")
	resetTrace       = flag.Bool("reset-trace", false, "remove a blktrace session left on -device (for example by a killed hot-clone or blktrace), then exit")
)

// setupBlkTrace starts a write-only blktrace session on the device and a
// reader per relay file that marks every written sector dirty in DST.
func setupBlkTrace(f *os.File, d blockDevice) {
	version := createBlkTrace(f, d)

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), BLKTRACESTART, 0)
	if errno != 0 {
		unix.Syscall(unix.SYS_IOCTL, f.Fd(), BLKTRACETEARDOWN, 0)
		log.Fatalf("failed to BLKTRACESTART -> %v", errno)
	}
	tracedFile = f
	log.Printf("Tracing writes to %s with blktrace protocol v%d", d.name, version)

	// Tear the trace down however the process gets stopped: Ctrl-C, kill,
	// the terminal or SSH session hanging up, or the far end of the output
	// pipe going away. Left alone, Go exits on SIGHUP and on SIGPIPE from a
	// write to stdout, leaving the trace running on the device.
	sigs := []os.Signal{os.Interrupt, unix.SIGTERM, unix.SIGPIPE}
	if !signal.Ignored(unix.SIGHUP) { // still survive hangups under nohup
		sigs = append(sigs, unix.SIGHUP)
	}
	c := make(chan os.Signal, 1)
	signal.Notify(c, sigs...)
	go func() {
		sig := <-c
		shutdownBlkTrace(f)
		log.Printf("Got %s signal. Aborting...", sig)
		os.Exit(1)
	}()

	// One reader per relay file actually present, rather than one per
	// runtime.NumCPU(): under a CPU affinity mask or cgroup cpuset, NumCPU
	// can be smaller than the number of online CPUs, and any unread relay
	// file would silently fill up and drop events.
	traceDir := fmt.Sprintf("/sys/kernel/debug/block/%s", d.name)
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
		go readBlkTraceEventFile(traceDir + "/" + name)
	}
	if traceFiles == 0 {
		shutdownBlkTrace(f)
		log.Fatalf("no per-CPU trace files found in %s", traceDir)
	}
}

// createBlkTrace sets up the trace session and returns the record format
// the kernel will write. BLKTRACESETUP2 (Linux 6.19+) comes first: since
// 6.19 the kernel silently drops events from sessions made with the older
// BLKTRACESETUP whenever their categories don't fit its 16-bit mask, and
// that includes every FUA write (journal commits, XFS log writes) and every
// write-zeroes.
func createBlkTrace(f *os.File, d blockDevice) int {
	setup2 := BLK_user_trace_setup2{
		Act_mask: blkTCWrite,
		Buf_size: uint32(*BlkTraceBufSize),
		Buf_nr:   uint32(*BlkTraceBufCount),
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), BLKTRACESETUP2, uintptr(unsafe.Pointer(&setup2)))
	if errno == 0 {
		return 2
	}
	if errno != unix.ENOTTY && errno != unix.EINVAL {
		blktraceSetupFailed(d, errno)
	}

	// An unknown ioctl: a kernel older than 6.19, where the original
	// protocol still records every write.
	if kernelAtLeast(6, 19) {
		log.Fatalf("BLKTRACESETUP2 failed (%v) on kernel %s, where the older BLKTRACESETUP would silently miss FUA and write-zeroes writes; refusing to image", errno, kernelRelease())
	}
	setup := BLK_user_trace_setup{
		Act_mask: blkTCWrite,
		Buf_size: uint32(*BlkTraceBufSize),
		Buf_nr:   uint32(*BlkTraceBufCount),
	}
	_, _, errno = unix.Syscall(unix.SYS_IOCTL, f.Fd(), BLKTRACESETUP, uintptr(unsafe.Pointer(&setup)))
	if errno != 0 {
		blktraceSetupFailed(d, errno)
	}
	return 1
}

// blktraceSetupFailed reports a failed setup. Nothing is torn down: setup
// failed, so any trace on the device belongs to someone else.
func blktraceSetupFailed(d blockDevice, errno unix.Errno) {
	if errno == unix.EBUSY {
		log.Fatalf("%v already has a blktrace session (blktrace, another tool, or one left behind by a killed hot-clone). Nothing was changed. If nothing else is tracing it, remove the stale session with: hot-clone -device %v -reset-trace", d.path, d.path)
	}
	log.Fatalf("failed to set up blktrace on %v -> %v", d.path, errno)
}

func kernelRelease() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	return unix.ByteSliceToString(u.Release[:])
}

// kernelAtLeast reports whether the running kernel is at least
// major.minor.
func kernelAtLeast(major, minor int) bool {
	var ma, mi int
	if _, err := fmt.Sscanf(kernelRelease(), "%d.%d", &ma, &mi); err != nil {
		return false
	}
	return ma > major || (ma == major && mi >= minor)
}

// resetTraceMain removes whatever blktrace session is on the device. It
// takes the device lock first, so it can't remove a running hot-clone's
// trace.
func resetTraceMain(d blockDevice) {
	lockDevice(d)
	f, err := os.Open(d.path)
	if err != nil {
		log.Fatalf("cannot open device %v - %v", d.path, err)
	}
	unix.Syscall(unix.SYS_IOCTL, f.Fd(), BLKTRACESTOP, 0)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), BLKTRACETEARDOWN, 0)
	switch errno {
	case 0:
		log.Printf("Removed the blktrace session on %v", d.path)
	case unix.EINVAL:
		log.Printf("No blktrace session was active on %v", d.path)
	default:
		log.Fatalf("failed to remove the blktrace session on %v -> %v", d.path, errno)
	}
}

var (
	traceReaders  sync.WaitGroup
	traceStopping int32
)

// stopAndDrainBlkTrace stops the kernel side of the trace and then waits
// for the readers to consume everything left in the relay buffers.
// BLKTRACETEARDOWN must not happen until this returns, or buffered events
// are lost.
func stopAndDrainBlkTrace(f *os.File) {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), BLKTRACESTOP, 0); errno != 0 {
		fatalf("BLKTRACESTOP failed (%v): the trace was stopped or removed by something else, cannot safely image device", errno)
	}
	// A CPU already recording an event when the stop lands finishes within
	// microseconds; give such events time to reach the relay buffers
	// before the readers treat running dry as the end.
	time.Sleep(100 * time.Millisecond)
	atomic.StoreInt32(&traceStopping, 1)
	traceReaders.Wait()
}

func shutdownBlkTrace(f *os.File) {
	unix.Syscall(unix.SYS_IOCTL, f.Fd(), BLKTRACESTOP, 0)
	unix.Syscall(unix.SYS_IOCTL, f.Fd(), BLKTRACETEARDOWN, 0)
}

// readBlkTraceEventFile consumes one per-CPU relay file, marking every
// written sector it reports dirty. It returns once tracing has stopped and
// the file is drained; anything else is fatal, because a reader that stops
// early means writes going untracked.
func readBlkTraceEventFile(path string) {
	defer traceReaders.Done()
	f, err := os.Open(path)
	if err != nil {
		fatalf("Cant open trace debugfs file %s ?! %v", path, err)
	}
	defer f.Close()

	// Room for the largest record: a 64-byte header, a cgroup id and a
	// payload of up to 64KiB.
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		// Read the flag before looking for data: if tracing had already
		// stopped and the buffer still comes up empty, nothing more can
		// arrive.
		stopping := atomic.LoadInt32(&traceStopping) != 0
		ev, size, err := nextTraceRecord(r)
		if err != nil {
			fatalf("trace reader %s failed (%v), cannot safely image device", path, err)
		}
		if size == 0 {
			if stopping {
				if r.Buffered() != 0 {
					fatalf("trace reader %s: trace ended in the middle of a record, cannot safely image device", path)
				}
				return
			}
			time.Sleep(time.Millisecond * 10)
			continue
		}
		if *dumpWrites {
			log.Printf("Trace %s", ev.describe())
		}
		if start, count, ok := ev.dirtyRange(); ok {
			DST.SetRange(start, count)
		}
		if ev.Error != 0 && ev.Action&(blkTCNotify<<blkTCShift) == 0 {
			log.Printf("Error %d reported for %s", ev.Error, ev.describe())
		}
		r.Discard(size)
	}
}
