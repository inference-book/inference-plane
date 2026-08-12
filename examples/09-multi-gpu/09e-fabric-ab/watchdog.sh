#!/bin/bash
# Cost watchdog for a paid run: destroy every iplane-* instance on a wall-clock
# deadline, whatever else has happened.
#
# It exists because the obvious protections do not hold. A shell `trap` does
# not run when the process is killed rather than exited, and it is deferred
# while a foreground command is in flight. A run that dies between renting a
# box and tearing it down leaves that box billing until someone notices.
#
# So this is deliberately independent: separate process, no shared state with
# the run, no reliance on signals or on the parent surviving.
#
# Usage: watchdog.sh <deadline_minutes> <logfile> [--dry-run]
#
# **Arm it before anything can be rented, and stop it when the run ends.** A
# watchdog left armed from a previous run swept a later run's boxes
# mid-measurement, which is not a hypothetical: it happened, and it cost a
# completed experiment. Tie its lifetime to the run.

set -uo pipefail
DEADLINE_MIN="${1:?deadline minutes required}"
LOG="${2:?logfile required}"
DRY="${3:-}"

VAST_API="${VAST_API_BASE:-https://console.vast.ai/api}"

log() { echo "$(date +%H:%M:%S) $*" >> "$LOG"; }

if [ -z "${VAST_API_KEY:-}" ]; then
  log "FATAL: VAST_API_KEY not visible; this watchdog cannot protect anything"
  exit 1
fi

# listInstances prints one "id label dph" line per iplane-* instance.
#
# Exit status is the contract, and it is the whole point of this function:
#
#   0  the API answered and this is the complete list (possibly empty)
#   1  the API could not be read, and NOTHING is known about what is running
#
# The original version collapsed those two into "print nothing, exit 0". A
# failed API read therefore looked exactly like a clean account, so the sweep
# concluded there was nothing to destroy and exited satisfied while a rented
# box kept billing. An instrument that reports success without measuring is
# worse than no instrument, because it is trusted.
listInstances() {
  local body
  body=$(curl -sS --max-time 20 -H "Authorization: Bearer $VAST_API_KEY" \
    "${VAST_API}/v1/instances/" 2>/dev/null) || return 1
  [ -n "$body" ] || return 1
  printf '%s' "$body" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)                      # unparseable is not-known, not empty
if isinstance(d, dict) and d.get('success') is False:
    sys.exit(1)                      # an API error object is not an empty list
ins = d.get('instances') if isinstance(d, dict) else d
if not isinstance(ins, list):
    sys.exit(1)
for i in ins:
    if isinstance(i, dict) and str(i.get('label') or '').startswith('iplane-'):
        print('%s %s %s' % (i.get('id'), i.get('label'), i.get('dph_total')))
" || return 1
  return 0
}

# listOrRetry tries several times before giving up, because a single failed
# read at exactly the wrong moment should not decide anything. A laptop
# resuming from sleep produces precisely that.
listOrRetry() {
  local out rc
  for attempt in 1 2 3 4 5; do
    if out=$(listInstances); then
      printf '%s' "$out"
      return 0
    fi
    log "list attempt ${attempt} failed"
    sleep 5
  done
  return 1
}

destroy() {
  local id="$1"
  for attempt in 1 2 3 4 5; do
    local code
    code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 20 -X DELETE \
      -H "Authorization: Bearer $VAST_API_KEY" \
      "${VAST_API}/v0/instances/${id}/" 2>/dev/null)
    log "destroy id=${id} attempt=${attempt} http=${code}"
    case "$code" in 200|204) return 0 ;; esac
    sleep 5
  done
  return 1
}

END=$(( $(date +%s) + DEADLINE_MIN * 60 ))
log "armed: sweeps iplane-* at $(date -r "$END" +%H:%M:%S) (${DEADLINE_MIN}m) dry_run=${DRY:-no}"

while true; do
  if found=$(listOrRetry); then
    [ -n "$found" ] && log "tracking: $(printf '%s' "$found" | tr '\n' ';')"
  else
    # Not fatal before the deadline -- there is time to recover -- but it must
    # be visible, because a watchdog that cannot see is not protecting anything.
    log "WARNING: cannot read the provider API; running blind"
  fi

  if [ "$(date +%s)" -ge "$END" ]; then
    log "DEADLINE REACHED, sweeping"

    if ! found=$(listOrRetry); then
      # The dangerous case, and the one the first version got wrong. Refuse to
      # exit clean: a non-zero exit surfaces as a failed task rather than a
      # silent success, and the operator gets told to look.
      log "ALERT: cannot read the provider API at the deadline. Instances may be running and billing."
      log "ALERT: check manually -- iplane instance list, or the provider console."
      exit 2
    fi

    if [ -z "$found" ]; then
      log "confirmed clean: the API answered and no iplane-* instances exist"
      exit 0
    fi

    if [ "$DRY" = "--dry-run" ]; then
      printf '%s\n' "$found" | while read -r id _label _dph; do
        [ -n "$id" ] && log "DRY-RUN would destroy id=${id}"
      done
      exit 0
    fi

    printf '%s\n' "$found" | while read -r id _label _dph; do
      [ -n "$id" ] && destroy "$id"
    done

    # Always re-verify, and treat an unreadable API here as failure too. The
    # sweep is only finished when something has confirmed it.
    sleep 15
    if ! still=$(listOrRetry); then
      log "ALERT: swept, but cannot confirm the account is clean. Check manually."
      exit 2
    fi
    if [ -n "$still" ]; then
      log "ALERT: still present after sweep: $(printf '%s' "$still" | tr '\n' ';')"
      exit 3
    fi
    log "sweep verified clean"
    exit 0
  fi
  sleep 30
done
