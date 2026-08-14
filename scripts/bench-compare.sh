#!/usr/bin/env bash
# Run the M5 comparison with both systems in containers on one machine.
#
# The protocol (docs/decisions/m5-comparison-protocol.md) requires it: on a Mac,
# a host-native spine reads and writes a real disk while a containerised Kafka
# reads and writes a virtualised one, and comparing those two would be comparing
# the hypervisor. Both sides here pay the same filesystem cost.
#
# Everything is torn down on exit, including on failure.

set -euo pipefail

# Pinned by digest, like every other image this repository runs. A floating tag
# would make the published comparison unreproducible.
KAFKA_IMAGE=apache/kafka@sha256:fbc7d7c428e3755cf36518d4976596002477e4c052d1f80b5b9eafd06d0fff2f

NETWORK=spine-bench
KAFKA=spine-bench-kafka
IMAGE=event-spine:bench

RECORDS=${RECORDS:-50000}
BATCH=${BATCH:-256}
SIZES=${SIZES:-64,1024}
SEED=${SEED:-1}

cleanup() {
    docker rm -f "$KAFKA" >/dev/null 2>&1 || true
    docker volume rm -f spine-bench-data >/dev/null 2>&1 || true
    docker network rm "$NETWORK" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

docker network create "$NETWORK" >/dev/null

# KRaft single node. The advertised listener has to be the container's name so
# the comparison container can reach it, and 9092 is published as well so a
# developer can point their own tools at it.
docker run -d --name "$KAFKA" --network "$NETWORK" -p 9092:9092 \
    -e KAFKA_NODE_ID=1 \
    -e KAFKA_PROCESS_ROLES=broker,controller \
    -e KAFKA_LISTENERS='PLAINTEXT://:9092,CONTROLLER://:9093' \
    -e KAFKA_ADVERTISED_LISTENERS="PLAINTEXT://${KAFKA}:9092" \
    -e KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER \
    -e KAFKA_CONTROLLER_QUORUM_VOTERS="1@${KAFKA}:9093" \
    -e KAFKA_LISTENER_SECURITY_PROTOCOL_MAP='CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT' \
    -e KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=1 \
    -e KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR=1 \
    -e KAFKA_TRANSACTION_STATE_LOG_MIN_ISR=1 \
    "$KAFKA_IMAGE" >/dev/null

echo "waiting for the broker" >&2
for _ in $(seq 60); do
    if docker exec "$KAFKA" /opt/kafka/bin/kafka-broker-api-versions.sh \
        --bootstrap-server "${KAFKA}:9092" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

docker build -q --target kafkacompare -t "$IMAGE" . >/dev/null

# A volume rather than a tmpfs: a log measured against RAM is not a log measured
# against a disk, and Kafka is writing to its container's filesystem.
docker volume create spine-bench-data >/dev/null

docker run --rm --network "$NETWORK" -v spine-bench-data:/data "$IMAGE" \
    --broker "${KAFKA}:9092" \
    --dir /data \
    --records "$RECORDS" \
    --batch "$BATCH" \
    --sizes "$SIZES" \
    --seed "$SEED"
