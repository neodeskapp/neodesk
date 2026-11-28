#!/usr/bin/env bash
# Checks of the image's start scripts (docker/), the Dockerfiles and the
# compose files, without Docker.
#
#   bash scripts/test-docker-entrypoint.sh
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

# L34: a database address in a log line shows where the database is and
# nothing of the credentials: no user name, no password, no query string
# (which may carry password=).
test_dsn_for_log_drops_credentials() (
    # shellcheck source=/dev/null
    . "$SCRIPT_ROOT/docker/dsn.sh"
    local got
    got=$(dsn_for_log 'postgres://bdesk:S3cr3tPw@postgres:5432/neodesk?sslmode=disable')
    [ "$got" = 'postgres://postgres:5432/neodesk' ] || fail "dsn_for_log gave '$got'"
    got=$(dsn_for_log 'postgresql://bdesk:p@ss:w0rd@db.example.net/bd')
    [ "$got" = 'postgresql://db.example.net/bd' ] || fail "a password with @ and : gave '$got'"
    got=$(dsn_for_log 'postgres://bdesk@db:5432/neodesk?password=Hunter2')
    [ "$got" = 'postgres://db:5432/neodesk' ] || fail "a password in the query gave '$got'"
    got=$(dsn_for_log 'host=db user=bdesk password=Hunter2')
    case "$got" in *Hunter2*|*bdesk*) fail "a key=value DSN gave '$got'" ;; esac
    got=$(dsn_for_log '/opt/rustdesk/db_v2.sqlite3')
    [ "$got" = '/opt/rustdesk/db_v2.sqlite3' ] || fail "a SQLite path gave '$got'"
    [ "$(dsn_host 'postgres://bdesk:p@ss:5@postgres:5433/bd')" = postgres ] || fail "dsn_host of a password with @"
    [ "$(dsn_port 'postgres://bdesk:p@ss:5@postgres:5433/bd')" = 5433 ] || fail "dsn_port of a password with @"
    [ "$(dsn_port 'postgres://bdesk:secret@postgres/bd')" = 5432 ] || fail "dsn_port without a port"
    [ "$(dsn_host 'postgres://bdesk:secret@[fd00::5]:5432/bd')" = 'fd00::5' ] || fail "dsn_host of an IPv6 address"
)

# L34: no start script prints a database address or password itself;
# every address goes through dsn_for_log.
test_scripts_print_no_raw_dsn() (
    local hits
    hits=$(grep -nE '(echo|printf).*\$\{?(DATABASE_URL|DB_URL|POSTGRESQL_URI|POSTGRESQL_PASS|POSTGRES_PASSWORD|PG_PASSWORD)' \
        "$SCRIPT_ROOT"/docker/*.sh "$SCRIPT_ROOT/install/neodesk-docker.sh" | grep -v 'dsn_for_log' || true)
    [ -z "$hits" ] || fail "a script prints a database address or password: $hits"
)

# The image copies dsn.sh next to the entrypoint, which sources it.
test_image_has_dsn_helper() (
    grep -q 'COPY docker/dsn.sh /docker/dsn.sh' "$SCRIPT_ROOT/docker/Dockerfile" \
        || fail "the Dockerfile does not copy docker/dsn.sh"
    grep -q '^\. /docker/dsn.sh' "$SCRIPT_ROOT/docker/entrypoint.sh" \
        || fail "docker/entrypoint.sh does not source /docker/dsn.sh"
)

# L35: the start scripts pass ENROLLMENT_MODE on as the first default and
# never mark it as forced; ENROLLMENT_MODE_ENV_OVERRIDE is the admin's to
# set, and every compose file of a server passes it through. A relay node
# enrolls no devices, so its compose file has no enrollment mode.
test_enrollment_mode_is_not_forced() (
    local hits f
    hits=$(grep -nE '^[^#]*ENROLLMENT_MODE_ENV_OVERRIDE=' "$SCRIPT_ROOT"/docker/*.sh || true)
    [ -z "$hits" ] || fail "a start script sets ENROLLMENT_MODE_ENV_OVERRIDE: $hits"
    for f in "$SCRIPT_ROOT"/install/docker-compose*.yml; do
        [ "$(basename "$f")" != docker-compose.relay-node.yml ] || continue
        grep -q 'ENROLLMENT_MODE_ENV_OVERRIDE=${ENROLLMENT_MODE_ENV_OVERRIDE:-}' "$f" \
            || fail "$(basename "$f") does not pass ENROLLMENT_MODE_ENV_OVERRIDE through"
    done
)

# The compose file the docs show first runs the published image, builds
# nothing and follows releases; every compose file says which one is which.
test_quick_compose_pulls_the_published_image() (
    local quick="$SCRIPT_ROOT/install/docker-compose.quick.single.yml" f other
    grep -q 'image: ghcr.io/neodeskapp/neodesk:${NEODESK_IMAGE_TAG:-latest}' "$quick" || fail "docker-compose.quick.single.yml does not run ghcr.io/neodeskapp/neodesk:latest"
    if grep -qE '^[[:space:]]+build:' "$quick"; then fail "docker-compose.quick.single.yml builds an image"; fi
    if grep -qE 'pull_policy:[[:space:]]*never' "$quick"; then fail "docker-compose.quick.single.yml never pulls"; fi
    for f in "$SCRIPT_ROOT"/install/docker-compose*.yml; do
        for other in docker-compose.quick.single.yml docker-compose.quick.single.macvlan.yml docker-compose.single.yml; do
            sed -n '1,20p' "$f" | grep -q "$other" || fail "the comment at the top of $(basename "$f") does not name $other"
        done
        if grep -qi 'ghcr.io/unitronix' "$f"; then fail "$(basename "$f") names the upstream image"; fi
    done
)

# MIGRATE-CLIENT-1: a stock RustDesk client without an API server
# setting calls the ID server's host on 21114; the server answers there
# too, so the image exposes it and the compose files with host ports
# publish it, and the manager opens it in the host firewall.
test_rustdesk_api_port_is_published() (
    local f
    for f in install/docker-compose.quick.single.yml install/docker-compose.single.yml; do
        grep -qE '^[[:space:]]+- "21114:21114"' "$SCRIPT_ROOT/$f" || fail "$f does not publish 21114"
    done
    grep -qE '^EXPOSE .* 21114( |$)' "$SCRIPT_ROOT/docker/Dockerfile" || fail "docker/Dockerfile does not expose 21114"
    grep -qE '^DOCKER_PORTS_TCP=\(21114 ' "$SCRIPT_ROOT/install/neodesk-docker.sh" || fail "neodesk-docker.sh does not open 21114"
)

# L79: the CDAP gateway listens on 21122 by default (A116), so the images
# expose it and the compose files with host ports publish it.
test_cdap_port_is_published() (
    local f
    for f in install/docker-compose.quick.single.yml install/docker-compose.single.yml; do
        grep -qE '^[[:space:]]+- "21122:21122"' "$SCRIPT_ROOT/$f" || fail "$f does not publish 21122"
    done
    for f in docker/Dockerfile docker/Dockerfile.server; do
        grep -qE '^EXPOSE .* 21122( |$)' "$SCRIPT_ROOT/$f" || fail "$f does not expose 21122"
    done
)

# L80: every compose file reads .env next to it (when there is one), so a
# setting needs no edit of the compose file, and the host address the
# console's ports are published on is a variable. DOCKER-QUICK-3: without
# NEODESK_CONSOLE_BIND the console is published on every address, IPv6
# too, as the client ports are (0.0.0.0 would be IPv4 only).
test_compose_reads_env_file() (
    local f
    for f in "$SCRIPT_ROOT"/install/docker-compose*.yml; do
        grep -A2 -E '^[[:space:]]+env_file:' "$f" | grep -qE 'path: \.env' || fail "$(basename "$f") does not read .env"
        grep -A2 -E '^[[:space:]]+env_file:' "$f" | grep -qE 'required: false' || fail "$(basename "$f") needs a .env to start"
    done
    for f in install/docker-compose.quick.single.yml install/docker-compose.single.yml; do
        grep -qF '"${NEODESK_CONSOLE_BIND:+${NEODESK_CONSOLE_BIND}:}${NEODESK_CONSOLE_PORT:-3375}:3375"' "$SCRIPT_ROOT/$f" || fail "$f fixes the console's bind address"
        grep -qF '"${NEODESK_CONSOLE_BIND:+${NEODESK_CONSOLE_BIND}:}${NEODESK_CONSOLE_HTTPS_PORT:-5443}:5443"' "$SCRIPT_ROOT/$f" || fail "$f fixes the console's HTTPS bind address"
        if grep -qE '^[^#]*0\.0\.0\.0:' "$SCRIPT_ROOT/$f"; then fail "$f publishes a port on IPv4 only"; fi
    done
)

# MIGRATE-CLIENT-3: tini runs as root and passes docker stop's SIGTERM on
# to the server, which runs as neodesk; with cap_drop: ALL that needs
# KILL, else tini dies with "Operation not permitted" and the server is
# killed without its shutdown. The health check runs every 10 seconds
# (DOCKER-QUICK-10). The relay node's service is relay-node.
test_neodesk_service_keeps_kill() (
    local f block service
    for f in "$SCRIPT_ROOT"/install/docker-compose*.yml; do
        service=neodesk
        [ "$(basename "$f")" != docker-compose.relay-node.yml ] || service=relay-node
        # The service: from its line to the next service
        block=$(awk -v s="  $service:" '$0 == s { on = 1; next } on && /^  [a-z]/ { exit } on' "$f")
        printf '%s
' "$block" | grep -qE '^      - ALL$' || fail "$(basename "$f"): the neodesk service keeps its capabilities"
        printf '%s
' "$block" | grep -qE '^      - KILL$' || fail "$(basename "$f"): the neodesk service has no KILL, so tini cannot pass SIGTERM on"
        printf '%s
' "$block" | grep -qE '^      interval: 10s$' || fail "$(basename "$f"): the health check does not run every 10 seconds"
    done
    grep -qE '^HEALTHCHECK --interval=10s ' "$SCRIPT_ROOT/docker/Dockerfile" || fail "the image's health check does not run every 10 seconds"
)

# HTTPS-3: Compose makes a container again only when its service's
# settings change, not when the content of a config does, so the Traefik
# service carries the domain of its routing in a label. HTTPS-6: the
# network takes IPv6 from NEODESK_IPV6. HTTPS-11: the contact is optional.
test_traefik_follows_the_domain() (
    local quick="$SCRIPT_ROOT/install/docker-compose.quick.single.yml" block
    block=$(awk '/^  traefik:/ { on = 1; next } on && /^[a-z]/ { exit } on && /^  [a-z]/ { exit } on' "$quick")
    printf '%s\n' "$block" | grep -qF -- '- sk.neode.traefik.domain=${NEODESK_DOMAIN:-console.invalid}' \
        || fail "the Traefik service does not carry the domain, so a new one is not applied"
    grep -qF 'rule: "Host(`${NEODESK_DOMAIN:-console.invalid}`)"' "$quick" || fail "the routing does not use NEODESK_DOMAIN"
    grep -qF 'enable_ipv6: ${NEODESK_IPV6:-false}' "$quick" || fail "the network does not take NEODESK_IPV6"
    grep -qF 'enable_ipv6: ${NEODESK_IPV6:-false}' "$SCRIPT_ROOT/install/docker-compose.single.yml" \
        || fail "the development compose file's network does not take NEODESK_IPV6"
    grep -qF -- '--certificatesresolvers.letsencrypt.acme.email=${LETSENCRYPT_EMAIL:-}' "$quick" || fail "the contact is not optional"
)

# L81: the health check reaches the API over HTTPS when TLS_API=Y and the
# console over HTTPS when HTTPS_ENABLED=true; the images and the compose
# files use it.
test_healthcheck_follows_tls() (
    local dir out
    dir=$(mktemp -d)
    # curl that answers only the URLs listed in $dir/up
    cat > "$dir/curl" <<'STUB'
#!/bin/sh
for a in "$@"; do url="$a"; done
grep -qxF "$url" "$(dirname "$0")/up"
STUB
    chmod +x "$dir/curl"
    printf '%s\n' https://127.0.0.1:21121/api/health http://127.0.0.1:3375/health > "$dir/up"
    PATH="$dir:$PATH" TLS_API=Y sh "$SCRIPT_ROOT/docker/healthcheck.sh" || fail "unhealthy with TLS_API=Y and the API on HTTPS"
    printf '%s\n' http://127.0.0.1:21121/api/health https://127.0.0.1:5443/health > "$dir/up"
    PATH="$dir:$PATH" HTTPS_ENABLED=true sh "$SCRIPT_ROOT/docker/healthcheck.sh" || fail "unhealthy with HTTPS_ENABLED=true and the console on 5443"
    printf '%s\n' http://127.0.0.1:21121/api/health > "$dir/up"
    if PATH="$dir:$PATH" sh "$SCRIPT_ROOT/docker/healthcheck.sh"; then fail "healthy without the console"; fi
    PATH="$dir:$PATH" CONSOLE_PORT=0 sh "$SCRIPT_ROOT/docker/healthcheck.sh" || fail "unhealthy with the console off"
    : > "$dir/up"
    if PATH="$dir:$PATH" CONSOLE_PORT=0 sh "$SCRIPT_ROOT/docker/healthcheck.sh"; then fail "healthy without the API"; fi
    rm -rf "$dir"
    for out in docker/Dockerfile docker/Dockerfile.server; do
        grep -qE '^[[:space:]]+CMD /usr/local/bin/neodesk-healthcheck' "$SCRIPT_ROOT/$out" || fail "$out does not use the health check script"
        grep -q 'COPY docker/healthcheck.sh /usr/local/bin/neodesk-healthcheck' "$SCRIPT_ROOT/$out" || fail "$out does not copy the health check script"
        grep -qE '^RUN chmod [+]x .*/usr/local/bin/neodesk-healthcheck' "$SCRIPT_ROOT/$out" || fail "$out does not make the health check script executable"
    done
    for out in "$SCRIPT_ROOT"/install/docker-compose*.yml; do
        if grep -A1 -E '^[[:space:]]+healthcheck:' "$out" | grep -q 'curl'; then fail "$(basename "$out") checks health with plain curl"; fi
    done
)

# L446: the images keep their data in /opt/neodesk/data and have nothing at
# /opt/rustdesk, no link either. The entrypoints and the image's helper
# scripts use the new folder; only docker/data-path.sh names the old one,
# for a compose file of 3.6.0-rc.4 or older that still mounts the data
# volume there (the start keeps the data where it is mounted).
test_images_keep_their_data_in_the_neodesk_folder() (
    local df f
    for df in docker/Dockerfile docker/Dockerfile.server; do
        if grep -nE '^[^#]*/opt/rustdesk' "$SCRIPT_ROOT/$df"; then fail "$df uses /opt/rustdesk"; fi
        grep -qx 'WORKDIR /opt/neodesk/data' "$SCRIPT_ROOT/$df" || fail "$df does not work in /opt/neodesk/data"
        grep -qF '"-key-file", "/opt/neodesk/data/id_ed25519"' "$SCRIPT_ROOT/$df" || fail "$df's server reads its key elsewhere"
        grep -qx 'COPY docker/data-path.sh /docker/data-path.sh' "$SCRIPT_ROOT/$df" || fail "$df lacks docker/data-path.sh"
    done
    grep -q '^    RUSTDESK_PATH=/opt/neodesk/data \\$' "$SCRIPT_ROOT/docker/Dockerfile" || fail "the image's RUSTDESK_PATH is not /opt/neodesk/data"
    for f in docker/entrypoint.sh docker/server-entrypoint.sh docker/bootstrap-admin-credentials.sh docker/show-admin-credentials.sh; do
        if grep -nE '^[^#]*/opt/rustdesk' "$SCRIPT_ROOT/$f"; then fail "$f uses /opt/rustdesk"; fi
    done
    for f in docker/entrypoint.sh docker/server-entrypoint.sh; do
        grep -q '^\. /docker/data-path\.sh$' "$SCRIPT_ROOT/$f" || fail "$f does not load docker/data-path.sh"
        grep -q '^choose_data_path$' "$SCRIPT_ROOT/$f" || fail "$f does not choose the data path"
    done
)

# L446: a compose file of 3.6.0-rc.4 or older mounts the data volume at
# /opt/rustdesk, which the image does not have. The start then keeps the
# data there: the paths into /opt/neodesk/data (settings, the server's
# -key-file) lead to the old mount, and the log says how to move it. With
# the volume at /opt/neodesk/data, or data there already, nothing changes.
test_old_data_mount_is_used_where_it_is() (
    local dir out
    dir=$(mktemp -d)
    out=$(sh -c '
        . "$1"
        NEW_DATA_PATH="$2/new" OLD_DATA_PATH="$2/old"
        mkdir -p "$NEW_DATA_PATH"
        RUSTDESK_PATH="$NEW_DATA_PATH" DB_PATH="$NEW_DATA_PATH/db_v2.sqlite3"
        choose_data_path
        echo "new: $DATA_PATH $DB_PATH $(on_data_path "$NEW_DATA_PATH/id_ed25519")"
        mkdir -p "$OLD_DATA_PATH"
        choose_data_path
        echo "old: $DATA_PATH $RUSTDESK_PATH $DB_PATH $(on_data_path "$NEW_DATA_PATH/id_ed25519") $(on_data_path -mode)"
        DB_PATH=/elsewhere/db.sqlite3
        choose_data_path
        echo "given: $DB_PATH"
        RUSTDESK_PATH="$NEW_DATA_PATH" DB_PATH="$NEW_DATA_PATH/db_v2.sqlite3"
        : > "$NEW_DATA_PATH/id_ed25519"
        choose_data_path
        echo "both: $DATA_PATH $DB_PATH"
    ' sh "$SCRIPT_ROOT/docker/data-path.sh" "$dir" 2>&1)
    printf '%s\n' "$out" | grep -qx "new: $dir/new $dir/new/db_v2.sqlite3 $dir/new/id_ed25519" \
        || fail "without an old mount the data path moved: $out"
    printf '%s\n' "$out" | grep -qx "old: $dir/old $dir/old $dir/old/db_v2.sqlite3 $dir/old/id_ed25519 -mode" \
        || fail "a volume at the old path is not used where it is: $out"
    printf '%s\n' "$out" | grep -q "mounted at $dir/old" || fail "the start does not say the volume is at the old path: $out"
    printf '%s\n' "$out" | grep -qx "given: /elsewhere/db.sqlite3" || fail "a path set elsewhere was changed: $out"
    printf '%s\n' "$out" | grep -qx "both: $dir/new $dir/new/db_v2.sqlite3" \
        || fail "data at /opt/neodesk/data was left for the old path: $out"
    rm -rf "$dir"
)

# L82: the images' binary knows its version, build date and commit: the
# Dockerfiles pass all three to the linker (the version from VERSION and
# the date of the build when no build argument names them), and the compose
# file that builds passes the commit on.
test_image_binary_knows_its_build() (
    local df
    for df in docker/Dockerfile docker/Dockerfile.server; do
        grep -qF -- '-X main.Commit=${NEODESK_COMMIT_SHA}' "$SCRIPT_ROOT/$df" || fail "$df builds without the commit"
        grep -qF -- '-X main.BuildDate=${built}' "$SCRIPT_ROOT/$df" || fail "$df builds without the build date"
        grep -qF -- 'version="${NEODESK_PRODUCT_VERSION:-$(tr -d' "$SCRIPT_ROOT/$df" || fail "$df does not take the version from VERSION"
    done
    grep -qE '^[[:space:]]+NEODESK_COMMIT_SHA: \$\{NEODESK_COMMIT_SHA:-\}' "$SCRIPT_ROOT/install/docker-compose.single.yml" \
        || fail "docker-compose.single.yml does not pass the commit to the build"
)

# L78: the credentials command's hints name the compose service neodesk.
test_credentials_hint_names_the_service() (
    if grep -q 'logs server' "$SCRIPT_ROOT/docker/show-admin-credentials.sh"; then
        fail "show-admin-credentials.sh points at a service called server"
    fi
    grep -q 'docker compose logs neodesk' "$SCRIPT_ROOT/docker/show-admin-credentials.sh" || fail "show-admin-credentials.sh does not point at docker compose logs neodesk"
)

# DOCKER-QUICK-8, DOCKER-QUICK-9: after the password change the hints
# work from any folder (the script by its full path) and keep a chosen
# password out of the shell history (read -rs, no password in the line).
test_credentials_hint_after_a_password_change() (
    local dir out
    dir=$(mktemp -d)
    printf 'The first password was replaced.\n' > "$dir/.admin_credentials.removed"
    if out=$(RUSTDESK_PATH="$dir" sh "$SCRIPT_ROOT/docker/show-admin-credentials.sh" 2>&1); then
        fail "show-admin-credentials.sh succeeded without a password"
    fi
    printf '%s\n' "$out" | grep -qx '    sudo bash /opt/neodesk/docker/neodesk-docker.sh reset-password' \
        || fail "the reset hint does not name the script by its full path: $out"
    printf '%s\n' "$out" | grep -qx '    read -rs NEWPASS' || fail "the hint does not read the password silently: $out"
    printf '%s\n' "$out" | grep -qF "printf '%s\\n' \"\$NEWPASS\" | docker compose exec -T -u neodesk neodesk neodesk-server reset-password admin" \
        || fail "the hint does not pipe the password read: $out"
    if printf '%s\n' "$out" | grep -q "'the new password'"; then fail "the hint puts a password into the command line: $out"; fi
    rm -rf "$dir"
)

# L77: after the console removed the first password (it leaves
# .admin_credentials.removed), the start makes no new credentials file,
# with or without ADMIN_PASSWORD; without the note a fresh volume gets one.
test_bootstrap_keeps_a_replaced_first_password_away() (
    local dir out
    dir=$(mktemp -d)
    : > "$dir/.admin_credentials.removed"
    out=$(cd "$dir" && RUSTDESK_PATH="$dir" DB_PATH="$dir/none.sqlite3" sh -c '. "$1"' sh "$SCRIPT_ROOT/docker/bootstrap-admin-credentials.sh" 2>&1)
    [ ! -e "$dir/.admin_credentials" ] || fail "the start made the first password again after it was replaced: $out"
    out=$(cd "$dir" && RUSTDESK_PATH="$dir" DB_PATH="$dir/none.sqlite3" ADMIN_PASSWORD=env-pass-123456 sh -c '. "$1"' sh "$SCRIPT_ROOT/docker/bootstrap-admin-credentials.sh" 2>&1)
    [ ! -e "$dir/.admin_credentials" ] || fail "the start wrote ADMIN_PASSWORD into a credentials file after the first password was replaced: $out"
    rm -f "$dir/.admin_credentials.removed"
    out=$(cd "$dir" && RUSTDESK_PATH="$dir" DB_PATH="$dir/none.sqlite3" sh -c '. "$1"' sh "$SCRIPT_ROOT/docker/bootstrap-admin-credentials.sh" 2>&1)
    grep -q '^Admin Password: ' "$dir/.admin_credentials" 2>/dev/null || fail "a fresh volume got no first password: $out"
    rm -rf "$dir"
)

# L83: the container makes the remote target vault key at its first start
# and keeps it in the data volume; guacd is an optional service in the
# container's own network (the server only talks to guacd on loopback).
test_remote_targets_work_out_of_the_box() (
    local ep="$SCRIPT_ROOT/docker/entrypoint.sh" quick="$SCRIPT_ROOT/install/docker-compose.quick.single.yml"
    grep -q 'VAULT_KEY_FILE="$DATA_PATH/.remote_target_vault_key"' "$ep" || fail "the entrypoint keeps no vault key in the volume"
    grep -q '^NEW_DATA_PATH=/opt/neodesk/data$' "$SCRIPT_ROOT/docker/data-path.sh" || fail "the entrypoint's data volume is not /opt/neodesk/data"
    grep -q 'export REMOTE_TARGET_VAULT_KEY' "$ep" || fail "the entrypoint does not pass the vault key to the server"
    if grep -nE '^[^#]*echo.*\$\{?REMOTE_TARGET_VAULT_KEY' "$ep"; then fail "the entrypoint prints the vault key"; fi
    sed -n '/^  guacd:/,/^  [a-z]/p' "$quick" | grep -q 'network_mode: "service:neodesk"' || fail "guacd does not share the container's network"
    sed -n '/^  guacd:/,/^  [a-z]/p' "$quick" | grep -qE '^ +- guacd$' || fail "guacd is not an optional service (profile guacd)"
)

# L244: the start banner names the ports the server listens on, as
# SIGNAL_PORT and RELAY_PORT move them, and a value that is no number
# does not stop the start.
test_banner_names_the_moved_ports() (
    local ep="$SCRIPT_ROOT/docker/entrypoint.sh" fn got
    fn=$(sed -n '/^server_ports() {/,/^}/p' "$ep")
    [ -n "$fn" ] || fail "the entrypoint has no server_ports for its banner"
    grep -q '^echo "  Server:       \$(server_ports)"' "$ep" || fail "the banner does not print server_ports"
    eval "$fn"
    got=$(SIGNAL_PORT=25116 RELAY_PORT=25117 server_ports)
    [ "$got" = "NAT test 25115, signal 25116, WebSocket signal 25118, relay 25117, WebSocket relay 25119, API 21121" ] \
        || fail "SIGNAL_PORT=25116 RELAY_PORT=25117 gave '$got'"
    got=$(unset SIGNAL_PORT RELAY_PORT; server_ports)
    [ "$got" = "NAT test 21115, signal 21116, WebSocket signal 21118, relay 21117, WebSocket relay 21119, API 21121" ] \
        || fail "the default ports gave '$got'"
    got=$(SIGNAL_PORT=abc server_ports) || fail "SIGNAL_PORT=abc stopped the banner"
    [ "$got" = "signal abc, relay 21117, API 21121" ] || fail "SIGNAL_PORT=abc gave '$got'"
)

test_banner_names_the_moved_ports

# L225: when the relay address is not found, the entrypoint says to set
# RELAY_SERVERS in .env, which every compose file reads, not in
# docker-compose.single.yml, which the published image's install does not
# have.
test_relay_hint_names_the_env_file() (
    local ep="$SCRIPT_ROOT/docker/entrypoint.sh" hints
    hints=$(grep -n 'Set RELAY_SERVERS' "$ep" || true)
    [ "$(printf '%s\n' "$hints" | grep -c .)" -ge 2 ] || fail "the entrypoint has fewer relay hints than before: $hints"
    if printf '%s\n' "$hints" | grep -q 'docker-compose'; then fail "a relay hint names a compose file: $hints"; fi
    if printf '%s\n' "$hints" | grep -qv '\.env'; then fail "a relay hint does not name .env: $hints"; fi
)

test_dsn_for_log_drops_credentials
test_scripts_print_no_raw_dsn
test_image_has_dsn_helper
test_enrollment_mode_is_not_forced
test_quick_compose_pulls_the_published_image
test_cdap_port_is_published
test_rustdesk_api_port_is_published
test_compose_reads_env_file
test_neodesk_service_keeps_kill
test_traefik_follows_the_domain
test_healthcheck_follows_tls
test_image_binary_knows_its_build
test_images_keep_their_data_in_the_neodesk_folder
test_old_data_mount_is_used_where_it_is
test_credentials_hint_names_the_service
test_credentials_hint_after_a_password_change
test_bootstrap_keeps_a_replaced_first_password_away
test_remote_targets_work_out_of_the_box
test_relay_hint_names_the_env_file
echo "Docker entrypoint checks passed"
