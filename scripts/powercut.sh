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
# 512 MiB because xfs refuses anything under 300, and both filesystems get the
# same device so the comparison is between journals rather than between sizes.
DISK_MB=${DISK_MB:-512}
IMAGE=${IMAGE:-event-spine:powercut}

# A guarantee that is only checked when someone remembers is one that decays,
# so this runs in `task ci` and refuses rather than skips when it cannot. A
# silent skip would leave the durability claim looking checked while nothing
# checks it, which is the failure mode the corpus's drift detector exists for.
if [ "${INSIDE_CONTAINER:-}" != "1" ]; then
    if ! docker run --rm --privileged "$IMAGE" true >/dev/null 2>&1 &&
       ! docker run --rm --privileged alpine:3.20 true >/dev/null 2>&1; then
        echo "powercut: this host cannot run a privileged container." >&2
        echo "The power-loss test needs loop devices and mount, which need" >&2
        echo "CAP_SYS_ADMIN. Run it somewhere that can before publishing any" >&2
        echo "durability claim — see docs/decisions/power-loss.md." >&2
        exit 1
    fi

    # The outer half: build the binary and re-enter privileged, because loop
    # devices and mounting need capabilities a normal container does not have.
    docker build -q --target powercut -t "$IMAGE" . >/dev/null
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

# The filesystems to cut the power under. The log's durability assumptions are
# about what POSIX promises rather than about any one implementation, and ext4
# and xfs journal differently enough that agreeing is worth something.
FILESYSTEMS=${FILESYSTEMS:-"ext4 xfs"}

survived=0
attempted=0
for FS in $FILESYSTEMS; do
echo "== $FS"
for cut in $(seq 1 "$CUTS"); do
    attempted=$((attempted + 1))
    rm -f /cut/disk.img /cut/snapshot.img /cut/acked
    truncate -s "${DISK_MB}M" /cut/disk.img

    LOOP=$(losetup --find --show --direct-io=on /cut/disk.img)
    case "$FS" in
        ext4) mkfs.ext4 -q -F "$LOOP" ;;
        xfs) mkfs.xfs -q -f "$LOOP" ;;
        *) echo "powercut: no mkfs rule for $FS" >&2; exit 1 ;;
    esac
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
    printf '%s cut %2d: ' "$FS" "$cut"
    if "$SPINE" powercut verify --dir /mnt/snapshot --acked /cut/acked; then
        survived=$((survived + 1))
    else
        umount -l /mnt/snapshot
        losetup -d "$SNAP"
        echo "FAILED on $FS at cut $cut"
        exit 1
    fi
    umount -l /mnt/snapshot
    losetup -d "$SNAP"
done
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

# One segment large enough that the run never rolls. Rolling syncs the
# outgoing segment in every mode — that is the invariant the second power-cut
# failure restored — so a rolling control would be flushed by the rolls and
# would survive the cut, reporting teeth the test does not have.
"$SPINE" powercut write --dir /mnt/disk --acked /cut/acked --never-sync --segment-bytes $((64 << 20)) &
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
if "$SPINE" powercut verify --dir /mnt/snapshot --acked /cut/acked --segment-bytes $((64 << 20)); then
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
echo "powercut: $survived of $attempted cuts across $FILESYSTEMS lost nothing acknowledged as durable"
