#!/usr/bin/env bash
set -euo pipefail

STATUS_URL="${OPENMESSAGE_STATUS_URL:-http://127.0.0.1:7007/api/status}"
SERVICE_NAME="${OPENMESSAGE_SERVICE_NAME:-openmessage.service}"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REFRESH_SCRIPT="${OPENMESSAGE_COOKIE_REFRESH_SCRIPT:-$SCRIPT_DIR/refresh-google-session-cookies-linux.py}"
FORCE=0

auth_error_pattern="SESSION_COOKIE_INVALID|invalid authentication credentials|HTTP 401"

if [[ "${1:-}" == "--force" ]]; then
  FORCE=1
fi

status_json=""
if status_json="$(curl -fsS --max-time 5 "$STATUS_URL" 2>/dev/null)"; then
  read -r paired connected last_error < <(
    python3 -c 'import json,sys
data=json.load(sys.stdin)
g=data.get("google") or {}
print(str(bool(g.get("paired"))).lower(), str(bool(g.get("connected"))).lower(), (g.get("last_error") or "").replace("\n", " "))' <<<"$status_json"
  )
else
  paired="false"
  connected="false"
  last_error="status endpoint unavailable"
fi

if [[ "$FORCE" != "1" ]]; then
  if [[ "$paired" != "true" ]]; then
    echo "OpenMessage is not paired; cookie watchdog has nothing to refresh."
    exit 0
  fi
  if grep -Eiq "$auth_error_pattern" <<<"$last_error"; then
    connected="false"
  fi
  if [[ "$connected" == "true" ]]; then
    main_pid="$(systemctl --user show "$SERVICE_NAME" -P MainPID 2>/dev/null || true)"
    if [[ "$main_pid" =~ ^[0-9]+$ ]] && [[ "$main_pid" != "0" ]]; then
      if journalctl --user "_PID=$main_pid" --since "${OPENMESSAGE_COOKIE_WATCHDOG_LOG_WINDOW:-3 minutes ago}" --no-pager 2>/dev/null | grep -Eiq "$auth_error_pattern"; then
        echo "Current OpenMessage process logged Google auth expiry; refreshing cookies."
        connected="false"
        last_error="recent service log contains Google auth expiry"
      fi
    fi
  fi
  if [[ "$connected" == "true" ]]; then
    echo "OpenMessage Google connection is healthy."
    exit 0
  fi
fi

echo "Refreshing OpenMessage Google cookies and restarting service. reason=${last_error:-forced}"
systemctl --user stop "$SERVICE_NAME" || true
"$REFRESH_SCRIPT" --quiet --no-backup
systemctl --user start "$SERVICE_NAME"
