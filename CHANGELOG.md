# Changelog

All notable changes to hot-clone are documented here.

## Unreleased

### Added

- Images now end with a trailer recording a CRC-32C checksum and byte count of the whole stream. `reassemble` verifies it and refuses to restore an image that is truncated, corrupted, or has trailing garbage. Images made by v1.0.1 or older have no trailer and can be restored with `-reassemble-allow-legacy` (without integrity verification).
- Restoring to a regular file now writes sparse output: all-zero regions are skipped and left as holes, so a mostly-empty disk restores to a much smaller file. Block-device targets are still written in full.
- Restoring to a block device now fails up front if the device is smaller than the image, instead of partway through the write.
- Partitions are now refused by default (blktrace reports absolute disk sectors, so partition imaging is unverified and can silently record stale data); pass `-allow-partition` to override.
- Device paths are resolved to their canonical kernel name, so `/dev/mapper/*` (LVM) and `/dev/disk/by-id/*` targets now work instead of failing to find their debugfs trace files.
- The page cache is dropped behind the main read pass (`posix_fadvise(DONTNEED)`) so imaging a large device no longer evicts the live system's hot data; pass `-keep-cache` to restore the old behaviour.
- Imaging can now run under a CPU affinity mask or cgroup cpuset safely: the trace readers are created per relay file actually present, not per `runtime.NumCPU()`, which previously left some CPUs' trace buffers undrained and guaranteed dropped events.
- The progress line now reports throughput and an ETA.

### Changed

- The restore path is now OS-independent, so `-reassemble` works on Windows and macOS as well as Linux (only imaging still requires Linux).
- The dirty-sector count shown each second is now maintained incrementally instead of scanning the entire bitmap, avoiding gigabytes of memory traffic per poll on multi-terabyte devices.
- The blktrace event channel buffer was enlarged and trace readers now reuse a payload buffer, reducing the chance of dropped events under write bursts.

### Removed

- Dropped the vendored 500-file fork of `golang.org/x/sys` in favour of the upstream module; the two kernel structs hot-clone needs are now defined directly. This also removes a class of line-ending corruption that came from that vendored tree.

### Fixed

- `reassemble` no longer logs spurious "short read" warnings, and reports the restored size on completion.

## [1.0.1] - 2026-07-12

- Renamed executable.

## [1.0.0] - 2026-07-12

### Fixed

- Every observed write is now marked dirty unconditionally. Previously, writes landing less than ~1MB behind the sequential read cursor were skipped on the assumption the read had already covered them, but a queued write can complete after the read passes it — this could leave stale data in the image permanently.
- blktrace is now fully drained before the dirty bitmap is snapshotted for the catch-up pass: tracing is stopped, all per-CPU trace-reader goroutines are given time to empty the kernel relay buffers, and only then is the dirty set read. Previously the snapshot could race events still in flight, losing them.
- The kernel's dropped-event counter is now checked synchronously right after the main pass and again after draining, not just once per second — drops in the final moments of a run were previously never observed, letting a corrupt image finish with no warning.
- Fixed a crash (division by zero) in the catch-up progress log when fewer than 10 sectors were dirty.
- Fixed an off-by-one in over-read sector numbering that misaligned the recorded sector for any data read past the device's reported size.
- Fatal exit paths and SIGTERM (previously only SIGINT/Ctrl-C) now tear down blktrace on the device before exiting, preventing a stuck trace that blocks the next run with EBUSY.
- Trace-reader goroutines now read each event's payload to completion and validate the trace magic number, instead of silently continuing after a short read could desynchronize the stream.
- `DirtySectorTracker` now uses atomic operations throughout instead of a mutex that some readers bypassed, removing a set of data races between the tracker, the periodic counter, and the catch-up iterator. It also no longer panics on a dirty sector beyond the tracked device size.
- Catch-up, over-read, and restore now check the result of every read/write instead of silently discarding some of them.
- `reassemble` now flushes and syncs the restored output before exiting.

### Performance

- `reassemble` now reads through a buffered reader instead of one byte per syscall for section headers — a large speedup for restores.
- The main imaging loop and the catch-up pass now reuse a single buffer instead of allocating a new one every iteration.
- Catch-up output is now buffered and flushed once instead of issuing a separate syscall per dirty sector.
