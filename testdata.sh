#!/usr/bin/env bash
# testdata.sh — hammers MongoDB with random inserts, updates, and deletes
# across multiple databases and collections.
#
# Usage: ./testdata.sh [uri] [delay_seconds] [workers]
#   uri            MongoDB connection URI  (default: mongodb://localhost:27017)
#   delay_seconds  Sleep between ops per worker (default: 0.1)
#   workers        Number of parallel workers  (default: 5)

URI="${1:-mongodb://localhost:27017}"
DELAY="${2:-0.1}"
WORKERS="${3:-5}"

DATABASES=("shopdb" "analyticsdb" "inventorydb" "userdb" "logsdb")
COLLECTIONS=("orders" "products" "events" "sessions" "payments" "reviews" "shipments")

NAMES=("Alice" "Bob" "Carol" "Dave" "Eve" "Frank" "Grace" "Hank" "Iris" "Jack")
CITIES=("New York" "London" "Tokyo" "Paris" "Berlin" "Sydney" "Toronto" "Lisbon" "Seoul" "Dubai")
STATUSES=("pending" "confirmed" "shipped" "delivered" "cancelled")
TAGS=("vip" "promo" "bulk" "retail" "wholesale" "express")

worker() {
  local id=$1
  while true; do
    DB="${DATABASES[$RANDOM % ${#DATABASES[@]}]}"
    COLL="${COLLECTIONS[$RANDOM % ${#COLLECTIONS[@]}]}"
    NAME="${NAMES[$RANDOM % ${#NAMES[@]}]}"
    CITY="${CITIES[$RANDOM % ${#CITIES[@]}]}"
    STATUS="${STATUSES[$RANDOM % ${#STATUSES[@]}]}"
    TAG="${TAGS[$RANDOM % ${#TAGS[@]}]}"
    AMOUNT=$(( RANDOM % 9900 + 100 ))
    QTY=$(( RANDOM % 50 + 1 ))
    OP=$(( RANDOM % 10 ))  # 0-6=insert (70%), 7-8=update (20%), 9=delete (10%)

    case $OP in
      0|1|2|3|4|5|6)
        mongosh --quiet "$URI" --eval "
          db = db.getSiblingDB('$DB');
          db.$COLL.insertOne({
            name:   '$NAME',
            city:   '$CITY',
            status: '$STATUS',
            tag:    '$TAG',
            amount: $AMOUNT,
            qty:    $QTY,
            ts:     new Date()
          });
        " &>/dev/null
        echo "[worker $id] insert → $DB.$COLL"
        ;;
      7|8)
        mongosh --quiet "$URI" --eval "
          db = db.getSiblingDB('$DB');
          const doc = db.$COLL.findOne();
          if (doc) {
            db.$COLL.updateOne(
              { _id: doc._id },
              { \$set: { status: '$STATUS', amount: $AMOUNT, ts: new Date() } }
            );
          }
        " &>/dev/null
        echo "[worker $id] update → $DB.$COLL"
        ;;
      9)
        mongosh --quiet "$URI" --eval "
          db = db.getSiblingDB('$DB');
          const doc = db.$COLL.findOne();
          if (doc) { db.$COLL.deleteOne({ _id: doc._id }); }
        " &>/dev/null
        echo "[worker $id] delete → $DB.$COLL"
        ;;
    esac

    sleep "$DELAY"
  done
}

echo "Starting $WORKERS workers — URI: $URI, delay: ${DELAY}s"
echo "Databases : ${DATABASES[*]}"
echo "Collections: ${COLLECTIONS[*]}"
echo "Press Ctrl+C to stop."
echo ""

# Launch workers in the background, track their PIDs
PIDS=()
for i in $(seq 1 "$WORKERS"); do
  worker "$i" &
  PIDS+=($!)
done

# Clean up all workers on exit
trap 'echo ""; echo "Stopping workers..."; kill "${PIDS[@]}" 2>/dev/null; wait; echo "Done."' EXIT INT TERM

wait
