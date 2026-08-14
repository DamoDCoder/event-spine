#!/usr/bin/env bash
# Cut the power under a running log, repeatedly, and check what survived.
#
# Every durability property this repository asserts has until now rested on
# sim.FS modelling a disk correctly. docs/decisions/m2-filesystem-model.md set
# the trigger explicitly: SIGKILL destroys a process, not a machine, so nothing
# had observed unsynced data actually being lost. This is the first test where a
# real kernel, real ext4, and a real block device decide.
#
# The cut works like this:
#
#   - the log lives on ext4 on a loop device opened with --direct-io, so a write
#     that reaches the device reaches the backing file and nothing sits in the
#     host's cache on its way;
#   - background writeback is made lazy, so data the log did not fsync stays
#     unwritten. That makes the test stricter, not weaker: more is at risk;
#   - the writer is SIGKILLed and the backing file is snapshotted immediately.
#     The snapshot holds what the block device actually received, which is the
#     definition of what survives a power cut;
#   - the snapshot is mounted — ext4 replays its journal exactly as it would
#     after a real crash — and the log is asked what it kept.
#
# What this does NOT test is stated in the M6 note: there is no physical drive
# here, so a drive's own write cache, FUA handling, and sector tearing are still
# unexercised. This is power loss below the filesystem, not below the disk.

set -euo pipefail

CUTS=${CUTS:-10}
DISK_MB=${DISK_MB:-256}
IMAGE=${IMAGE:-event-spine:powercut}

if [ "${INSIDE_CONTAINER:-}" != "1" ]; then
    # The outer half: build the binary and re-enter privileged, because loop
    # devices and mounting need capabilities a normal container does not have.
    docker build -q --target build -t "$IMAGE" . >/dev/null
    exec docker run --rm --privileged \
        -e INSIDE_CONTAINER=1 -e CUTS="$CUTS" -e DISK_MB="$DISK_MB" \
        -v "$(pwd)/scripts:/scripts:ro" \
        "$IMAGE" /scripts/powercut.sh
fi

# ------------------------------------------------------------ inside the machine

SPINE=/out/spine
[ -x "$SPINE" ] || SPINE=$(command -v spine)

mkdir -p /cut /mnt/disk /mnt/snapshot

# Writeback stays lazy for the duration, so unsynced data is still unwritten
# when the power goes.
sysctl -q -w vm.dirty_expire_centisecs=360000 vm.dirty_writeback_centisecs=360000

survived=0
for cut in $(seq 1 "$CUTS"); do
    rm -f /cut/disk.img /cut/snapshot.img /cut/acked
    truncate -s "${DISK_MB}M" /cut/disk.img

    LOOP=$(losetup --find --show --direct-io=on /cut/disk.img)
    mkfs.ext4 -q -F "$LOOP"
    mount "$LOOP" /mnt/disk

    "$SPINE" powercut write --dir /mnt/disk --acked /cut/acked &
    writer=$!

    # A different moment in each round, so the cut lands at a different point in
    # the log's work: mid-append, mid-sync, mid-segment-roll.
    sleep "0.$((RANDOM % 5 + 2))"

    kill -9 "$writer" 2>/dev/null || true
    wait "$writer" 2>/dev/null || true

    # The power goes here. Everything the device received is in the backing
    # file; everything still in the page cache is gone, which is what a snapshot
    # taken with O_DIRECT reads reproduces.
    dd if=/cut/disk.img of=/cut/snapshot.img bs=4M iflag=direct status=none

    umount -l /mnt/disk
    losetup -d "$LOOP"

    SNAP=$(losetup --find --show /cut/snapshot.img)
    mount "$SNAP" /mnt/snapshot
    printf 'cut %2d: ' "$cut"
    if "$SPINE" powercut verify --dir /mnt/snapshot --acked /cut/acked; then
        survived=$((survived + 1))
    else
        umount -l /mnt/snapshot
        losetup -d "$SNAP"
        echo "FAILED at cut $cut"
        exit 1
    fi
    umount -l /mnt/snapshot
    losetup -d "$SNAP"
done

# ------------------------------------------------------------ the negative control
#
# A power cut that never loses anything is not a power cut, and every passing
# round above would be evidence about writeback rather than about fsync. This
# round claims durability without syncing: it has to fail.

rm -f /cut/disk.img /cut/snapshot.img /cut/acked
truncate -s "${DISK_MB}M" /cut/disk.img
LOOP=$(losetup --find --show --direct-io=on /cut/disk.img)
mkfs.ext4 -q -F "$LOOP"
mount "$LOOP" /mnt/disk

"$SPINE" powercut write --dir /mnt/disk --acked /cut/acked --never-sync &
writer=$!
sleep 0.4
kill -9 "$writer" 2>/dev/null || true
wait "$writer" 2>/dev/null || true

dd if=/cut/disk.img of=/cut/snapshot.img bs=4M iflag=direct status=none
umount -l /mnt/disk
losetup -d "$LOOP"

SNAP=$(losetup --find --show /cut/snapshot.img)
mount "$SNAP" /mnt/snapshot
printf 'control: '
if "$SPINE" powercut verify --dir /mnt/snapshot --acked /cut/acked; then
    umount -l /mnt/snapshot
    losetup -d "$SNAP"
    echo
    echo "powercut: NO TEETH — data that was never synced survived the cut."
    echo "The cut is not modelling power loss, so the rounds above prove nothing."
    exit 1
fi
umount -l /mnt/snapshot
losetup -d "$SNAP"
echo "         the control failed as it must: unsynced data did not survive"

echo
echo "powercut: $survived of $CUTS cuts lost nothing that was acknowledged as durable"
