#!/bin/sh
# Health check of the container: the API answers on 21121 and, unless the
# console is off (CONSOLE_PORT=0), the web console answers. Each is asked
# over HTTPS first when it serves HTTPS (TLS_API for the API, HTTPS_ENABLED
# for the console), then over plain HTTP, so a missing certificate (the
# server then falls back to HTTP) does not make the container unhealthy.
#
# A relay node (MODE=node) has no API and no console: it is healthy when
# its relay takes connections on the TCP port (RELAY_PORT, 21117) and the
# WebSocket port two above it.

if [ "${MODE:-}" = node ]; then
    relay_port="${RELAY_PORT:-21117}"
    case "$relay_port" in
        ''|*[!0-9]*) exit 1 ;;
    esac
    nc -z -w 5 127.0.0.1 "$relay_port" || exit 1
    nc -z -w 5 127.0.0.1 "$((relay_port + 2))" || exit 1
    exit 0
fi

is_on() {
    case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
        y|yes|true|1|on) return 0 ;;
    esac
    return 1
}

answers() {
    curl -fsk --max-time 5 -o /dev/null "$1"
}

api_https="https://127.0.0.1:21121/api/health"
api_http="http://127.0.0.1:21121/api/health"
if is_on "$TLS_API"; then
    answers "$api_https" || answers "$api_http" || exit 1
else
    answers "$api_http" || answers "$api_https" || exit 1
fi

console_port="${CONSOLE_PORT:-3375}"
[ "$console_port" = "0" ] && exit 0
console_http="http://127.0.0.1:${console_port}/health"
console_https="https://127.0.0.1:${HTTPS_PORT:-5443}/health"
if is_on "$HTTPS_ENABLED"; then
    answers "$console_https" || answers "$console_http" || exit 1
else
    answers "$console_http" || answers "$console_https" || exit 1
fi
exit 0
