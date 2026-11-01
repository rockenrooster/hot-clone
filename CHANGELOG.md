# Changelog

All notable changes to hot-clone are documented here.

## Unreleased

### Fixed

- **Linux 6.19 and newer:** tracing is now set up with `BLKTRACESETUP2` when the kernel has it. Since 6.19 the kernel silently drops events from sessions made with the older `BLKTRACESETUP` whenever their categories don't fit its 16-bit mask — which includes every FUA write (filesystem journal commits, XFS log writes) and every write-zeroes — so those writes were never re-read and images taken on 6.19+ kernels could contain stale data. Older kernels keep the original protocol, and hot-clone refuses to fall back to it on 6.19+.
- Write tracking tested the wrong bit of each trace event's action field, so only some event types (request issue/insert) counted. Devices that never issue requests — device-mapper (LVM, dm-crypt) and md RAID — had almost none of their writes tracked, and on any device a write already in flight when tracing started could be missed. Every write event now counts.
- The device is now read with `O_DIRECT`. A whole disk's page cache isn't kept coherent with writes made through its partitions, filesystems or stacked devices, so buffered reads could return stale cached data — including the catch-up re-read of a sector the trace had just marked dirty. v1.0.0 and v1.0.1 left the whole device in the page cache as they read it, so their images could be affected wherever data was still cached at catch-up time.
- Writes during the catch-up pass are now tracked. Catch-up runs in rounds with tracing still on, each re-reading what was written since the previous one; only a final small round runs after tracing stops. Previously every write during the whole catch-up went unrecorded.
- SIGPIPE (the far end of a `| ssh` or `| zstd` pipe going away) and SIGHUP (the terminal or SSH session hanging up) now tear down the blktrace session before exiting. Previously they killed hot-clone with the trace still active on the device, and the next run failed with "device or resource busy". Running under `nohup` still works.
- hot-clone no longer removes a blktrace session it didn't create. When setup failed because the device already had one — another hot-clone, or an admin's `blktrace` — it tore that session down, silently breaking the other tracer. It now refuses and leaves it alone; a lock also stops two hot-clone runs on the same device.
- Losing the trace mid-run now aborts the image: an unreadable dropped-event counter (it disappears when the trace is torn down), a failed stop, or a reader error while draining used to be logged or ignored.
- `reassemble` no longer logs spurious "short read" warnings, and reports the restored size on completion.

### Added

- Images now end with a trailer recording a CRC-32C checksum and byte count of the whole stream. `reassemble` verifies it and refuses to restore an image that is truncated, corrupted, or has trailing garbage. Images made by v1.0.1 or older have no trailer and can be restored with `-reassemble-allow-legacy` (without integrity verification).
- Restoring to a regular file now writes sparse output: all-zero regions that no earlier part of the image wrote are left as holes, so a mostly-empty disk restores to a much smaller file. Block-device targets are still written in full.
- Restoring to a block device now fails up front if the device is smaller than the image, instead of partway through the write.
- `-reset-trace` removes a blktrace session left behind on a device (for example by a `kill -9`), and refuses while another hot-clone is working on it.
- Partitions are now refused by default (blktrace reports absolute disk sectors, so partition imaging is unverified and can silently record stale data); pass `-allow-partition` to override.
- Device paths are resolved to their canonical kernel name, so `/dev/mapper/*` (LVM) and `/dev/disk/by-id/*` targets now work instead of failing to find their debugfs trace files.
- Imaging can now run under a CPU affinity mask or cgroup cpuset safely: the trace readers are created per relay file actually present, not per `runtime.NumCPU()`, which previously left some CPUs' trace buffers undrained and guaranteed dropped events.
- The progress line now reports throughput and an ETA.
- End-to-end tests in CI image loop devices and a device-mapper device under concurrent writes, FUA writes, write-zeroes and discards, on kernels 6.8 and 7.0, and check every restored image byte for byte; they also cover the trace-handling fixes above.

### Changed

- Catch-up re-reads each run of consecutive dirty sectors as one section of up to 1 MiB, instead of one 512-byte section per sector.
- Trace events are decoded from a buffered reader without reflection, each event marks its sector range a bitmap word at a time, and the per-CPU readers mark sectors directly instead of funnelling every event through one goroutine, so heavy write loads are less likely to overflow the trace buffers.
- The dirty-sector count shown each second is now maintained incrementally instead of scanning the entire bitmap, avoiding gigabytes of memory traffic per poll on multi-terabyte devices.
- `-print-writes` now names each trace event's type and categories; the old decoder mislabelled them.

### Removed

- Dropped the vendored 500-file fork of `golang.org/x/sys` in favour of the upstream module; the kernel structs hot-clone needs are now defined directly. This also removes a class of line-ending corruption that came from that vendored tree.

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
