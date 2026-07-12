# hot-clone

Create a block-level image of a **live Linux block device**, including the disk containing the running root filesystem.

`hot-clone` reads the device while using Linux `blktrace` to track sectors changed during the copy. After the main pass finishes, it rereads those sectors and appends the corrected data to the backup.

> [!WARNING]
> Imaging a mounted filesystem is not the same as creating an atomic snapshot. Prefer LVM, ZFS, Btrfs, DRBD, filesystem snapshots, or an offline image when available.

## Features

* Images mounted or actively changing block devices
* Supports imaging the running root disk
* Tracks writes that occur during the copy
* Aborts if `blktrace` reports dropped events
* Streams backup data through standard output
* Supports files, SSH, pipes, and compression
* Restores to a raw image file or block device

## Requirements

* Linux
* Root access when creating an image
* A kernel and device that support `blktrace`
* Go 1.16 or newer when building from source

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

Progress is written to standard error, while the backup stream is written to standard output.

Example:

```text
Read 3.7 GiB -- 360 Dirty sectors (0 event drops)
Catching up 360/360 sectors
Done
```

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

A `.hc` file is not a directly mountable raw disk image. Reassemble it first.

### Restore to an image file

```bash
./hot-clone \
    -reassemble sdb.hc \
    -reassemble-output sdb.img
```

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
      Block device to image

-reassemble string
      Backup file to restore

-reassemble-output string
      Destination image file or block device

-blktrace.bufcount int
      Number of trace buffers

-blktrace.bufsize int
      Size of each trace buffer

-print-writes
      Log observed writes

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

This fork includes additional fixes for:

* Lost or delayed write events
* Dropped-event detection
* Trace cleanup after errors or termination
* Dirty-sector tracking races
* Read and write error handling
* Restore and catch-up performance

See [CHANGELOG.md](CHANGELOG.md) for details.

## Project history

Based on the original [`benjojo/hot-clone`](https://github.com/benjojo/hot-clone) project.

## License

Licensed under the [MIT License](LICENSE).
