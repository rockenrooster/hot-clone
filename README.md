# hot-clone

Create a block-level image of a **live Linux block device**, including the disk containing the running root filesystem.

`hot-clone` reads the device while using Linux `blktrace` to track sectors changed during the copy. After the main pass finishes, it rereads those sectors and appends the corrected data to the backup — in rounds while tracing is still running, so writes that land during the catch-up are caught too. Each image ends with a checksum trailer so a truncated or corrupted backup is detected on restore rather than silently trusted.

> [!WARNING]
> Imaging a mounted filesystem is not the same as creating an atomic snapshot. Prefer LVM, ZFS, Btrfs, DRBD, filesystem snapshots, or an offline image when available.

## Features

* Images mounted or actively changing block devices
* Supports imaging the running root disk
* Tracks writes that occur during the copy
* Aborts if `blktrace` reports dropped events
* Detects truncated or corrupted backups on restore (checksum trailer)
* Restores empty regions sparsely, and refuses a destination that is too small
* Streams backup data through standard output
* Supports files, SSH, pipes, and compression
* Restores to a raw image file or block device

## Requirements

* Linux, root access, and a kernel/device that support `blktrace`, with debugfs mounted at `/sys/kernel/debug`
* Kernel lockdown must be off: it blocks the trace files hot-clone reads, and Debian and Ubuntu turn it on under Secure Boot (check `/sys/kernel/security/lockdown`)
* Go 1.21 or newer when building from source
* Roughly 256 MiB of RAM per TB of device being imaged (for the dirty-sector bitmap)

## Build

```bash
git clone https://github.com/rockenrooster/hot-clone.git
cd hot-clone
go build -o hot-clone .
```

Verify the binary:

```bash
./hot-clone --help
```

## Create a backup

Identify the correct device:

```bash
lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINTS,MODEL
```

Create the backup:

```bash
sudo ./hot-clone -device /dev/sdb > sdb.hc
```

Image a **whole disk** (`/dev/sdb`), not a partition (`/dev/sdb1`). `blktrace` reports absolute disk sectors, so partition imaging is unverified and could record stale data; partitions are refused unless you pass `-allow-partition`. LVM volumes (`/dev/mapper/...`) and `/dev/disk/by-id/...` paths are supported — they are resolved to their kernel device automatically.

Progress is written to standard error, while the backup stream is written to standard output.

Example:

```text
Tracing writes to sdb with blktrace protocol v2
Read 3.7 GiB of 3.7 GiB (210.3 MiB/s, ETA 0s) -- 360 dirty sectors
Catch-up round 1: re-read 360 dirty sectors (180.0 KiB)
Catch-up final round, tracing stopped: re-read 4 dirty sectors (2.0 KiB)
Done
```

Linux 6.19 and newer get blktrace protocol v2; older kernels use v1.

Do not save the backup onto the device being imaged.

### Compress the backup

```bash
sudo ./hot-clone -device /dev/sdb |
    zstd -T0 -10 -o sdb.hc.zst
```

### Send it over SSH

```bash
sudo ./hot-clone -device /dev/sdb |
    ssh backup@example-host 'cat > /srv/backups/sdb.hc'
```

## Restore or reassemble

A `.hc` file is not a directly mountable raw disk image. Reassemble it first. Restoring verifies the image's checksum trailer and aborts if the backup is truncated or corrupted.

> [!NOTE]
> Images created by hot-clone v1.0.1 or older have no checksum trailer. Restore them with `-reassemble-allow-legacy` (integrity is not verified in that mode).

### Restore to an image file

```bash
./hot-clone \
    -reassemble sdb.hc \
    -reassemble-output sdb.img
```

All-zero regions are written as holes, so the resulting `sdb.img` is a sparse file that only consumes space for data actually present on the source disk.

Inspect the resulting image:

```bash
fdisk -l sdb.img
```

### Restore to a block device

> [!CAUTION]
> This overwrites the destination device.

```bash
sudo ./hot-clone \
    -reassemble sdb.hc \
    -reassemble-output /dev/sdc
```

Confirm the destination with `lsblk` before running the command.

## Common options

```text
-device string
      Block device to image (whole disk; see -allow-partition)

-allow-partition
      Permit imaging a partition (unsafe; see the note above)

-reset-trace
      Remove a blktrace session left on -device (for example by a killed
      run), then exit

-reassemble string
      Backup file to restore ("-" reads from standard input)

-reassemble-output string
      Destination image file or block device

-reassemble-allow-legacy
      Restore an image that has no checksum trailer (v1.0.1 or older),
      without integrity verification

-blktrace.bufcount int
      Number of trace buffers

-blktrace.bufsize int
      Size of each trace buffer

-print-writes
      Log every trace event

-verbose
      Enable additional logging
```

## Heavy write workloads

If the source device is receiving many writes, increase the trace buffers:

```bash
sudo ./hot-clone \
    -device /dev/sdb \
    -blktrace.bufcount 64 \
    -blktrace.bufsize 262144 \
    > sdb.hc
```

If trace events are dropped, the backup is aborted because it can no longer be considered safe. Delete the incomplete output and retry with less disk activity or larger buffers.

## Stale blktrace sessions

hot-clone removes its blktrace session however it exits, short of `kill -9` or a crash. If a run reports that the device already has a blktrace session and nothing else is tracing it, remove the leftover session:

```bash
sudo ./hot-clone -device /dev/sdb -reset-trace
```

`-reset-trace` refuses to run while another hot-clone is working on the same device.

## Limitations

`hot-clone` does not create a true point-in-time snapshot.

For better results:

* Stop databases and write-heavy services
* Run `sync` before starting
* Minimize activity during imaging
* Save the backup to another device or remote system
* Reassemble and test the image after creation
* Keep an independent backup made using another method

## Changes in this fork

This fork adds, on top of the original:

* blktrace protocol v2 on Linux 6.19+, where the old protocol silently drops FUA and write-zeroes writes
* Write tracking that works on device-mapper (LVM, dm-crypt) and md devices
* Direct (`O_DIRECT`) reads, so stale page-cache data never reaches the image
* Catch-up rounds while tracing is still on, so writes during the catch-up are caught
* Checksum trailer so truncated or corrupted backups are caught on restore
* Sparse restore, and refusal of a too-small destination device
* Partition-imaging guard and LVM / `by-id` device resolution
* Safety under CPU affinity masks / cgroup cpusets
* Reliability fixes: lost/delayed write events, dropped-event detection, trace cleanup after errors, signals or a broken output pipe, leaving other tracers' sessions alone, dirty-sector tracking races, read/write error handling
* Restore and catch-up performance
* End-to-end tests in CI that image real block devices under concurrent writes
* Removal of the vendored `golang.org/x/sys` fork in favour of upstream

See [CHANGELOG.md](CHANGELOG.md) for details.

## Project history

Based on the original [`benjojo/hot-clone`](https://github.com/benjojo/hot-clone) project.

## License

Licensed under the [MIT License](LICENSE).
