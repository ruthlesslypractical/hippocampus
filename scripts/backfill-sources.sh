#!/bin/sh
# One-off: backfill 'sources' field on existing 3h summaries.
# For each summary:3h:<track>:<date-hour>, finds entries in that track+window
# and writes their IDs as the 'sources' HASH field.
#
# Safe to re-run (overwrites sources field idempotently).
# Requires: redis-cli accessible at $REDIS_ADDR (default 10.11.1.100:6379)

REDIS_ADDR="${REDIS_ADDR:-10.11.1.100:6379}"
REDIS_PASS="${REDIS_PASS:-}"

rcli() {
    if [ -n "$REDIS_PASS" ]; then
        redis-cli -h "${REDIS_ADDR%%:*}" -p "${REDIS_ADDR##*:}" -a "$REDIS_PASS" --no-auth-warning "$@"
    else
        redis-cli -h "${REDIS_ADDR%%:*}" -p "${REDIS_ADDR##*:}" "$@"
    fi
}

# Get all summary:3h entry IDs from the tag set
SUMMARY_IDS=$(rcli SMEMBERS "tag:summary:3h" | sort)
TOTAL=$(echo "$SUMMARY_IDS" | wc -l | tr -d ' ')
COUNT=0
FILLED=0
SKIPPED=0

echo "Backfilling sources on $TOTAL 3h summaries..."

for ID in $SUMMARY_IDS; do
    COUNT=$((COUNT + 1))

    # Check if sources already populated
    EXISTING=$(rcli HGET "entry:$ID" "sources")
    if [ -n "$EXISTING" ]; then
        SKIPPED=$((SKIPPED + 1))
        continue
    fi

    # Parse track and time window from ID: summary:3h:<track>:<YYYY-MM-DD-HH>
    # Handle track names with spaces by extracting date from the end
    DATE_HOUR=$(echo "$ID" | grep -oE '[0-9]{4}-[0-9]{2}-[0-9]{2}-[0-9]{2}$')
    if [ -z "$DATE_HOUR" ]; then
        echo "  SKIP (can't parse date): $ID"
        continue
    fi

    # Track is everything between "summary:3h:" and ":<date>"
    TRACK=$(echo "$ID" | sed "s/^summary:3h://;s/:${DATE_HOUR}$//")

    # Compute window start/end as unix timestamps
    # Date format: YYYY-MM-DD-HH -> YYYY-MM-DD HH:00:00
    DATE_PART=$(echo "$DATE_HOUR" | cut -c1-10)
    HOUR_PART=$(echo "$DATE_HOUR" | cut -c12-13)
    WINDOW_START=$(date -j -f "%Y-%m-%d %H:%M:%S" "${DATE_PART} ${HOUR_PART}:00:00" "+%s" 2>/dev/null)
    if [ -z "$WINDOW_START" ]; then
        echo "  SKIP (can't parse timestamp): $ID"
        continue
    fi
    WINDOW_END=$((WINDOW_START + 10800))  # +3 hours

    # Find entries in this track within the time window
    # Use ZRANGEBYSCORE on timeline, then filter by track tag membership
    TIMELINE_IDS=$(rcli ZRANGEBYSCORE timeline "$WINDOW_START" "$WINDOW_END")
    if [ -z "$TIMELINE_IDS" ]; then
        continue
    fi

    # Filter to entries that have this track tag
    SOURCES=""
    for EID in $TIMELINE_IDS; do
        # Check if entry is in the track's tag set
        IS_MEMBER=$(rcli SISMEMBER "tag:track:$TRACK" "$EID")
        if [ "$IS_MEMBER" = "1" ]; then
            if [ -n "$SOURCES" ]; then
                SOURCES="${SOURCES},${EID}"
            else
                SOURCES="$EID"
            fi
        fi
    done

    if [ -n "$SOURCES" ]; then
        rcli HSET "entry:$ID" "sources" "$SOURCES" > /dev/null
        SRC_COUNT=$(echo "$SOURCES" | tr ',' '\n' | wc -l | tr -d ' ')
        FILLED=$((FILLED + 1))
        # Progress every 20
        if [ $((FILLED % 20)) -eq 0 ]; then
            echo "  [$COUNT/$TOTAL] $ID -> $SRC_COUNT sources"
        fi
    fi
done

echo ""
echo "Done: $FILLED filled, $SKIPPED already had sources, $TOTAL total."
