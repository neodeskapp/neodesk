#!/bin/sh
# NeoDesk Server — Docker Entrypoint
# Fixes volume file permissions before dropping to non-root user
set -e

# shellcheck source=/dev/null
. /ensure-app-user.sh
ensure_neodesk_user

# A relay node (MODE=node, or neodesk-server node): no database, no
# console and no admin account. Its data folder (NODE_DATA_DIR, the
# volume at /opt/neodesk/data) holds node.key and node.json; only neodesk
# may read it.
node_mode() {
    [ "${MODE:-}" = node ] && return 0
    for arg do
        [ "$arg" = node ] && return 0
    done
    return 1
}
if node_mode "$@"; then
    export MODE=node
    export NODE_DATA_DIR="${NODE_DATA_DIR:-/opt/neodesk/data}"
    if [ "$(id -u)" = "0" ]; then
        mkdir -p "$NODE_DATA_DIR"
        chown -R neodesk:neodesk "$NODE_DATA_DIR" 2>/dev/null || true
        chmod 700 "$NODE_DATA_DIR" 2>/dev/null || true
        set -- su-exec neodesk "$@"
    fi
    exec "$@"
fi

# The data volume, at /opt/neodesk/data, or where a compose file of
# 3.6.0-rc.4 or older still mounts it; the server's arguments follow
# shellcheck source=docker/data-path.sh
. /docker/data-path.sh
choose_data_path
for arg do
    shift
    set -- "$@" "$(on_data_path "$arg")"
done
DATA_DIR="$DATA_PATH"

# Normalize the Go database path before credential bootstrap and startup.
# Split Compose files should set DB_URL explicitly, but custom deployments may
# only set DB_PATH or neither. Keep bootstrap inspection and Go on the same
# absolute volume-backed path instead of relying on the container WORKDIR.
if [ -z "${DB_URL:-}" ]; then
    case "${DB_TYPE:-sqlite}" in
        postgres|postgresql)
            export DB_URL="${DATABASE_URL:-${DB_PATH:-${RUSTDESK_PATH:-$DATA_DIR}/db_v2.sqlite3}}"
            ;;
        *)
            export DB_URL="${DB_PATH:-${RUSTDESK_PATH:-$DATA_DIR}/db_v2.sqlite3}"
            ;;
    esac
fi

# Fix ownership before bootstrap writes .admin_credentials.
if [ "$(id -u)" = "0" ]; then
    chown -R neodesk:neodesk "$DATA_DIR" 2>/dev/null || true
    if [ -f "$DATA_DIR/id_ed25519" ]; then
        chmod 600 "$DATA_DIR/id_ed25519"
        chown neodesk:neodesk "$DATA_DIR/id_ed25519"
    fi
fi

# Load shared bootstrap helpers before any app-owned path checks. The helper
# definitions are also used by the panel and enrollment checks below.
# shellcheck source=/docker/bootstrap-admin-credentials.sh
. /docker/bootstrap-admin-credentials.sh

# SQLite Docker: legacy auth.db is optional. Fresh installs keep panel
# identities in the primary db_v2.sqlite3; only explicitly legacy deployments
# need to wait for a separate auth.db.
panel_auth_db_ready() {
    case "${DB_URL:-}" in
        postgres://*|postgresql://*) return 0 ;;
    esac
    auth_path="${AUTH_DB_PATH:-}"
    if [ -z "$auth_path" ]; then
        return 0
    fi
    if ! file_exists_as_neodesk "$auth_path" && [ "${SQLITE_AUTH_DB_MODE:-}" != "legacy" ]; then
        echo "Panel auth.db not present — using the primary SQLite database"
        return 0
    fi
    if file_exists_as_neodesk "$auth_path"; then
        echo "Panel auth.db ready: $auth_path"
        return 0
    fi
    echo "Waiting for panel auth.db at $auth_path (console container)..."
    retries=0
    max_retries=90
    while ! file_exists_as_neodesk "$auth_path" && [ "$retries" -lt "$max_retries" ]; do
        sleep 2
        retries=$((retries + 1))
    done
    if ! file_exists_as_neodesk "$auth_path"; then
        echo "WARN: panel auth.db not found after ${max_retries} attempts — RustDesk folders/groups may be unavailable"
        return 0
    fi
    echo "Panel auth.db ready: $auth_path"
    return 0
}
panel_auth_db_ready

# Default enrollment policy for fresh deployments.
# A volume without a server key or SQLite database is treated as a fresh
# install and defaults to "managed" (stock RustDesk clients are queued for
# operator approval). Pre-existing volumes keep their current behavior
# (Go default "open", or whatever the panel persisted in the database).
# The server stores the mode of its first start and keeps the one saved in
# the console from then on; ENROLLMENT_MODE is only that first default
# (ENROLLMENT_MODE_ENV_OVERRIDE=Y forces it at every start).
if [ -z "${ENROLLMENT_MODE:-}" ]; then
    ENROLLMENT_SENTINEL="$DATA_DIR/.enrollment_initialized"
    if ! file_exists_as_neodesk "$ENROLLMENT_SENTINEL"; then
        if file_exists_as_neodesk "$DATA_DIR/db_v2.sqlite3" \
            || file_exists_as_neodesk "$DATA_DIR/id_ed25519"; then
            : # pre-existing volume — keep current enrollment policy
        else
            export ENROLLMENT_MODE="managed"
            echo "Enrollment: managed (fresh install — new devices need approval)"
        fi
        touch "$ENROLLMENT_SENTINEL" 2>/dev/null || true
    fi
fi

# Drop privileges and re-exec, in an older place of the data volume from
# there (the image works in /opt/neodesk/data)
if [ "$DATA_PATH" != "$NEW_DATA_PATH" ]; then
    set -- sh -c 'cd "$0" && exec "$@"' "$DATA_PATH" "$@"
fi
if [ "$(id -u)" = "0" ]; then
    exec su-exec neodesk "$@"
else
    exec "$@"
fi
