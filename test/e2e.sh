#!/usr/bin/env bash
# End-to-end tests for hot-clone against real block devices.
#
# Images loop devices (and a device-mapper linear device stacked on one, the
# way LVM is) while they are being written to, restores each image both to a
# file and to a block device, and checks the result is byte-identical to the
# device. Also checks how the blktrace session is handled: teardown on
# SIGPIPE, leaving a foreign trace alone, -reset-trace, and aborting when the
# trace is torn down mid-run.
#
# Needs root, a kernel with blktrace (CONFIG_BLK_DEV_IO_TRACE) and debugfs,
# plus losetup, dmsetup, blkdiscard, blktrace and python3.
#
# Usage: sudo test/e2e.sh path/to/hot-clone

set -uo pipefail

HC=$(realpath "$1")
WORK=$(mktemp -d)
SIZE_MB=128
SLOW=$((4 << 20))   # bytes/s the image pipe drains at, so imaging outlasts the writer
FAST=$((1 << 30))

loops=()
maps=()
pids=()
failures=()

cleanup() {
	local p m l
	for p in "${pids[@]}"; do kill -9 "$p" 2>/dev/null; done
	for m in "${maps[@]}"; do dmsetup remove "$m" 2>/dev/null; done
	for l in "${loops[@]}"; do
		blktrace -k -d "$l" >/dev/null 2>&1
		losetup -d "$l" 2>/dev/null
	done
	rm -rf "$WORK"
}
trap cleanup EXIT

pass() { echo "PASS: $1"; }
fail() {
	failures+=("$1")
	echo "FAIL: $1"
	if [ -n "${2:-}" ] && [ -f "$2" ]; then
		echo "---- last lines of $2 ----"
		tail -n 25 "$2"
		echo "----"
	fi
}

kernel_at_least() { # kernel_at_least MAJOR MINOR
	local maj min
	IFS=. read -r maj min _ <<<"$(uname -r)"
	((maj > $1 || (maj == $1 && min >= $2)))
}

# new_loop NAME: a loop device over a file of random data -> $REPLY
new_loop() {
	local f="$WORK/$1.img"
	dd if=/dev/urandom of="$f" bs=1M count=$SIZE_MB status=none
	REPLY=$(losetup --find --show --direct-io=on "$f")
	loops+=("$REPLY")
}

# new_map NAME LOWER: a bio-based dm-linear device over LOWER, like an LVM
# logical volume -> $REPLY
new_map() {
	local name="hc-e2e-$1-$$"
	echo "0 $(blockdev --getsz "$2") linear $2 0" | dmsetup create "$name"
	maps+=("$name")
	udevadm settle 2>/dev/null
	REPLY="/dev/mapper/$name"
}

trace_active() { # trace_active DEV: does DEV's request queue have a blktrace session?
	[ -e "/sys/kernel/debug/block/$(basename "$(realpath "$1")")/dropped" ]
}

# writer OPS TARGET...: random 16 KiB writes, O_DSYNC writes (REQ_FUA),
# write-zeroes and discards against the targets
writer() {
	local ops=$1
	shift
	local targets=("$@")
	local blocks=$(($(blockdev --getsz "$1") / 8 - 8))
	local i t off
	for ((i = 0; i < ops; i++)); do
		t=${targets[RANDOM % ${#targets[@]}]}
		off=$(((RANDOM * 32768 + RANDOM) % blocks))
		case $((i % 4)) in
		0) dd if=/dev/urandom of="$t" bs=4k count=4 seek="$off" oflag=direct conv=notrunc status=none ;;
		1) dd if=/dev/urandom of="$t" bs=4k count=1 seek="$off" oflag=direct,dsync conv=notrunc status=none ;;
		# -f: the loop device may be held open exclusively by a dm device on top
		2) blkdiscard -f -z -o $((off * 4096)) -l 16384 "$t" ;;
		3) blkdiscard -f -o $((off * 4096)) -l 16384 "$t" 2>/dev/null ||
			blkdiscard -f -z -o $((off * 4096)) -l 16384 "$t" ;;
		esac
		sleep 0.02
	done
}

throttle() { # throttle BYTES_PER_SEC: copy stdin to stdout no faster than that
	python3 -c '
import sys, time
rate = float(sys.argv[1])
src, dst = sys.stdin.buffer, sys.stdout.buffer
start, total = time.monotonic(), 0
while True:
    chunk = src.read1(1 << 20)
    if not chunk:
        break
    dst.write(chunk)
    total += len(chunk)
    ahead = total / rate - (time.monotonic() - start)
    if ahead > 0:
        time.sleep(ahead)
dst.flush()
' "$1"
}

# image NAME DEV RATE: image DEV into $WORK/NAME.hc (log in NAME.log);
# returns hot-clone's exit status
image() {
	timeout 600 "$HC" -device "$2" 2>"$WORK/$1.log" | throttle "$3" >"$WORK/$1.hc"
	return "${PIPESTATUS[0]}"
}

# verify NAME DEV: NAME.hc, restored to a file (sparse path) and to a block
# device (full-write path), must both equal DEV's current content
verify() {
	local name=$1 dev=$2 ok=0 diffs target
	dd if="$dev" of="$WORK/$name.final" bs=1M iflag=direct status=none
	if ! "$HC" -reassemble "$WORK/$name.hc" -reassemble-output "$WORK/$name.restored" 2>>"$WORK/$name.log"; then
		echo "  restore to file failed"
		ok=1
	elif ! cmp -s "$WORK/$name.restored" "$WORK/$name.final"; then
		diffs=$(cmp -l "$WORK/$name.restored" "$WORK/$name.final" 2>/dev/null | wc -l)
		echo "  restore to file differs from device in $diffs bytes"
		ok=1
	fi
	truncate -s "$(blockdev --getsize64 "$dev")" "$WORK/$name.target"
	target=$(losetup --find --show "$WORK/$name.target")
	loops+=("$target")
	if ! "$HC" -reassemble "$WORK/$name.hc" -reassemble-output "$target" 2>>"$WORK/$name.log"; then
		echo "  restore to block device failed"
		ok=1
	else
		dd if="$target" of="$WORK/$name.restored-dev" bs=1M iflag=direct status=none
		if ! cmp -s "$WORK/$name.restored-dev" "$WORK/$name.final"; then
			diffs=$(cmp -l "$WORK/$name.restored-dev" "$WORK/$name.final" 2>/dev/null | wc -l)
			echo "  restore to block device differs from device in $diffs bytes"
			ok=1
		fi
	fi
	return $ok
}

expect_protocol() { # the log must name the blktrace protocol this kernel needs
	local want=v1
	kernel_at_least 6 19 && want=v2
	grep -q "blktrace protocol $want" "$WORK/$1.log"
}

# Concurrent imaging tests -------------------------------------------------

# Whole disk, written both directly and through a device stacked on it (how
# a filesystem on a partition or LV reaches the disk).
test_whole_disk() {
	local s=whole-disk dev map w rc
	new_loop $s && dev=$REPLY
	new_map $s "$dev" && map=$REPLY
	writer 120 "$dev" "$map" &
	w=$!
	pids+=("$w")
	image $s "$dev" $SLOW
	rc=$?
	wait "$w"
	if [ $rc -ne 0 ]; then fail "$s: hot-clone exited $rc" "$WORK/$s.log"; return; fi
	if verify $s "$dev"; then pass $s; else fail "$s: image does not match device" "$WORK/$s.log"; fi
	if ! expect_protocol $s; then fail "$s: log does not name the blktrace protocol $(uname -r) needs" "$WORK/$s.log"; fi
}

# A bio-based device (dm-linear, like an LVM volume) imaged directly.
test_bio_based() {
	local s=bio-based dev map w rc
	new_loop $s && dev=$REPLY
	new_map $s "$dev" && map=$REPLY
	writer 120 "$map" &
	w=$!
	pids+=("$w")
	image $s "$map" $SLOW
	rc=$?
	wait "$w"
	if [ $rc -ne 0 ]; then fail "$s: hot-clone exited $rc" "$WORK/$s.log"; return; fi
	if verify $s "$map"; then pass $s; else fail "$s: image does not match device" "$WORK/$s.log"; fi
}

# The whole-disk page cache is not coherent with writes made through a
# stacked device; imaging must read the disk, not stale cached pages.
test_stale_cache() {
	local s=stale-cache dev map
	new_loop $s && dev=$REPLY
	new_map $s "$dev" && map=$REPLY
	cat "$dev" >/dev/null
	writer 60 "$map"
	if ! image $s "$dev" $FAST; then fail "$s: hot-clone failed" "$WORK/$s.log"; return; fi
	if verify $s "$dev"; then pass $s; else fail "$s: image contains stale page-cache data" "$WORK/$s.log"; fi
}

# blktrace session handling -------------------------------------------------

# The far end of the output pipe going away must still tear the trace down.
test_sigpipe() {
	local s=sigpipe dev rc
	new_loop $s && dev=$REPLY
	timeout 120 "$HC" -device "$dev" 2>"$WORK/$s.log" | head -c 1048576 >/dev/null
	rc=${PIPESTATUS[0]}
	sleep 1
	if [ "$rc" -eq 0 ]; then fail "$s: hot-clone reported success on a broken pipe" "$WORK/$s.log"; return; fi
	if trace_active "$dev"; then fail "$s: blktrace session left behind after SIGPIPE" "$WORK/$s.log"; return; fi
	pass $s
}

# Another tracer's session must be left alone; a stale one can be cleared
# with -reset-trace.
test_foreign_trace() {
	local s=foreign-trace dev bt i
	new_loop $s && dev=$REPLY
	blktrace -d "$dev" -o - >/dev/null 2>&1 &
	bt=$!
	pids+=("$bt")
	for ((i = 0; i < 50; i++)); do
		trace_active "$dev" && break
		sleep 0.2
	done
	if ! trace_active "$dev"; then fail "$s: could not start a blktrace session to test against"; return; fi

	if timeout 60 "$HC" -device "$dev" >"$WORK/$s.hc" 2>"$WORK/$s.log"; then
		fail "$s: hot-clone imaged a device that already had a blktrace session" "$WORK/$s.log"
		return
	fi
	if ! trace_active "$dev" || ! kill -0 "$bt" 2>/dev/null; then
		fail "$s: hot-clone tore down another tracer's session" "$WORK/$s.log"
		return
	fi

	kill -9 "$bt"
	wait "$bt" 2>/dev/null
	if ! trace_active "$dev"; then fail "$s: expected a stale session after killing blktrace"; return; fi
	if ! timeout 60 "$HC" -device "$dev" -reset-trace 2>>"$WORK/$s.log"; then
		fail "$s: -reset-trace failed" "$WORK/$s.log"
		return
	fi
	if trace_active "$dev"; then fail "$s: -reset-trace left the session in place" "$WORK/$s.log"; return; fi
	if ! image $s "$dev" $FAST; then fail "$s: imaging after -reset-trace failed" "$WORK/$s.log"; return; fi
	if verify $s "$dev"; then pass $s; else fail "$s: image after -reset-trace does not match" "$WORK/$s.log"; fi
}

# Losing the trace mid-run must abort the image rather than finish it with
# writes untracked.
test_external_teardown() {
	local s=external-teardown dev job rc
	new_loop $s && dev=$REPLY
	(
		timeout 600 "$HC" -device "$dev" 2>"$WORK/$s.log" | throttle $SLOW >"$WORK/$s.hc"
		echo "${PIPESTATUS[0]}" >"$WORK/$s.rc"
	) &
	job=$!
	pids+=("$job")
	sleep 4
	blktrace -k -d "$dev" >/dev/null 2>&1
	wait "$job"
	rc=$(cat "$WORK/$s.rc")
	if [ "$rc" -eq 0 ]; then fail "$s: hot-clone finished the image after its trace was removed" "$WORK/$s.log"; return; fi
	if "$HC" -reassemble "$WORK/$s.hc" -reassemble-output "$WORK/$s.restored" 2>/dev/null; then
		fail "$s: the aborted image restored as if complete"
		return
	fi
	pass $s
}

# ---------------------------------------------------------------------------

echo "kernel: $(uname -r)"
mountpoint -q /sys/kernel/debug || mount -t debugfs debugfs /sys/kernel/debug
echo "lockdown: $(cat /sys/kernel/security/lockdown 2>/dev/null || echo unavailable)"
modprobe dm_mod 2>/dev/null

test_whole_disk
test_bio_based
test_stale_cache
test_sigpipe
test_foreign_trace
test_external_teardown

echo
if [ ${#failures[@]} -ne 0 ]; then
	echo "${#failures[@]} e2e test(s) failed:"
	printf '  %s\n' "${failures[@]}"
	exit 1
fi
echo "all e2e tests passed"
