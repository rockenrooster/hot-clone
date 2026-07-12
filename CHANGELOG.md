# Changelog

All notable changes to hot-clone are documented here.

## [1.0.1] - 2026-07-12

- Renamed executable.

## Unreleased

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
