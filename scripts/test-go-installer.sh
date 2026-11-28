#!/usr/bin/env bash
# Unit checks of neodesk.sh: reading the units, writing the one unit, the
# settings, the update and its way back, the commands it installs. Runs
# without systemd: the functions work on files in a temporary folder.
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

# load: the installer's functions with every path in a temporary folder.
load() {
    set --
    # shellcheck disable=SC1091
    source "$SCRIPT_ROOT/install/neodesk.sh"
    TEST_DIR=$(mktemp -d)
    SERVER_UNIT="$TEST_DIR/neodesk-server.service"
    LEGACY_CONSOLE_UNIT="$TEST_DIR/neodesk-console.service"
    RUSTDESK_PATH="$TEST_DIR/opt-neodesk"
    CONSOLE_PATH="$TEST_DIR/console"
    mkdir -p "$RUSTDESK_PATH" "$CONSOLE_PATH"
    LOG_FILE=/dev/null
    print_info() { :; }
}

# fake_current_binary: an installed binary that checks release signatures
# itself (it names verify-release in its help), as this release's does.
fake_current_binary() {
    printf '#!/bin/sh\n# verify-release --manifest FILE (the commands it has)\nexit 0\n' > "$RUSTDESK_PATH/neodesk-server"
    chmod +x "$RUSTDESK_PATH/neodesk-server"
}

# fake_handing_binary: an installed binary whose command line hands the
# installer's options to neodesk-setup (it names it), as 3.6.0-rc.5's does.
fake_handing_binary() {
    printf '#!/bin/sh\n# verify-release --manifest FILE; the options of neodesk-setup\nexit 0\n' > "$RUSTDESK_PATH/neodesk-server"
    chmod +x "$RUSTDESK_PATH/neodesk-server"
}

# commands_in_test_dir: the installer's manager (neodesk-setup), its old
# place (/usr/local/sbin/neodesk up to 3.6.0-rc.4) and the command line
# (neodesk) in the temporary folder.
commands_in_test_dir() {
    MANAGER_COMMAND="$TEST_DIR/sbin/neodesk-setup"
    OLD_MANAGER_COMMAND="$TEST_DIR/sbin/neodesk"
    CLI_COMMAND="$TEST_DIR/bin/neodesk"
}

# The units an install with the Node.js console has (neodesk.sh 3.5.221).
write_node_era_units() {
    cat > "$SERVER_UNIT" <<'UNIT'
[Service]
Type=simple
User=root
EnvironmentFile=-/opt/NeoDeskConsole/.env
Environment=AUTH_DB_PATH=/opt/NeoDeskConsole/data/auth.db
Environment=MESH_ENABLED=Y
Environment=SIGNAL_PORT=21116
Environment=GO_API_PORT=21114
Environment=CONSOLE_PORT=0
Environment="P2P_FIRST=Y"
Environment=P2P_FALLBACK_MS=2000 SAME_NAT_RELAY=Y
ExecStart=/opt/neodesk/neodesk-server -mode all -relay-servers 203.0.113.7 -db "postgres://u:p$$w%%d@localhost:5432/bd" -key-file /opt/neodesk/id_ed25519 -api-port 21121 -signal-rate-limit-per-ip 20 -init-admin-pass S3cr$$t -tls-cert /opt/neodesk/ssl/neodesk.crt -tls-key /opt/neodesk/ssl/neodesk.key -tls-signal -tls-relay -tls-api
UNIT
    cat > "$LEGACY_CONSOLE_UNIT" <<'UNIT'
[Service]
User=neodesk
EnvironmentFile=-/opt/NeoDeskConsole/.env
ExecStart=/usr/bin/node server.js
Environment=NODE_ENV=production
Environment=PORT=8080
Environment=HOST=0.0.0.0
Environment=NODE_EXTRA_CA_CERTS=/opt/neodesk/ssl/neodesk.crt
Environment=NEODESK_SERVER_BINARY=/usr/local/bin/neodesk-server
UNIT
}

test_unit_environment() (
    load
    write_node_era_units
    local got
    got=$(unit_environment "$SERVER_UNIT" | tr '\n' ' ')
    [ "$got" = "AUTH_DB_PATH=/opt/NeoDeskConsole/data/auth.db MESH_ENABLED=Y SIGNAL_PORT=21116 GO_API_PORT=21114 CONSOLE_PORT=0 P2P_FIRST=Y P2P_FALLBACK_MS=2000 SAME_NAT_RELAY=Y " ] \
        || fail "unit_environment: $got"
    [ "$(unit_env_value "$LEGACY_CONSOLE_UNIT" PORT)" = 8080 ] || fail "unit_env_value PORT"
    [ -z "$(unit_env_value "$LEGACY_CONSOLE_UNIT" MISSING)" ] || fail "unit_env_value of a missing key"
    rm -rf "$TEST_DIR"
)

test_exec_args() (
    load
    write_node_era_units
    local args
    args=$(unit_exec_args "$SERVER_UNIT")
    [ "$args" = "-mode all -relay-servers 203.0.113.7 -key-file /opt/neodesk/id_ed25519 -api-port 21114 -signal-rate-limit-per-ip 20 -tls-cert /opt/neodesk/ssl/neodesk.crt -tls-key /opt/neodesk/ssl/neodesk.key -tls-signal -tls-relay" ] \
        || fail "unit_exec_args: $args"
    [ "$(unit_exec_flag_value "$SERVER_UNIT" -db)" = 'postgres://u:p$w%d@localhost:5432/bd' ] || fail "-db value"
    [ "$(unit_exec_flag_value "$SERVER_UNIT" -init-admin-pass)" = 'S3cr$t' ] || fail "-init-admin-pass value"
    rm -rf "$TEST_DIR"
)

test_write_server_unit() (
    load
    write_server_unit "-mode all -api-port 21114" ""
    local unit="$SERVER_UNIT"
    grep -qx "User=neodesk" "$unit" || fail "unit user"
    grep -qx "Environment=NEODESK_SUPERVISOR=systemd" "$unit" || fail "NEODESK_SUPERVISOR"
    grep -qx "Environment=NEODESK_ENV_FILE=$CONSOLE_PATH/.env" "$unit" || fail "NEODESK_ENV_FILE"
    grep -qx "SuccessExitStatus=3" "$unit" || fail "SuccessExitStatus"
    if grep -q 'ENROLLMENT_MODE' "$unit"; then fail "the unit sets the enrollment mode"; fi
    grep -qx "Restart=always" "$unit" || fail "Restart"
    grep -qx "ExecStart=$RUSTDESK_PATH/neodesk-server -mode all -api-port 21114" "$unit" || fail "ExecStart"
    if grep -q 'CONSOLE_PORT\|EnvironmentFile\|__EMPTY__\|AmbientCapabilities' "$unit"; then
        fail "unexpected line in the unit: $(grep 'CONSOLE_PORT\|EnvironmentFile\|__EMPTY__\|AmbientCapabilities' "$unit")"
    fi
    is_go_only_install || fail "is_go_only_install on the written unit"

    # A console port below 1024 needs the capability; extra settings stay
    printf 'CONSOLE_PORT=80\n' > "$CONSOLE_PATH/.env"
    write_server_unit "-mode all" "Environment=TRUST_PROXY=Y"
    grep -qx "AmbientCapabilities=CAP_NET_BIND_SERVICE" "$unit" || fail "AmbientCapabilities for port 80"
    grep -qx "Environment=TRUST_PROXY=Y" "$unit" || fail "extra environment"
    [ "$(server_unit_extra_environment)" = "Environment=TRUST_PROXY=Y" ] || fail "server_unit_extra_environment: $(server_unit_extra_environment)"
    rm -rf "$TEST_DIR"
)

# L68: the signal and relay ports live in .env, where the admin changes
# them; the unit does not set them, and an update moves the ports an older
# unit set into .env instead of setting 21116 and 21117 again.
test_update_keeps_the_admins_ports() (
    load
    systemctl() { :; }
    print_info() { :; }
    write_server_unit "-mode all -api-port 21114" ""
    if grep -q 'SIGNAL_PORT\|RELAY_PORT' "$SERVER_UNIT"; then fail "the unit sets $(grep 'SIGNAL_PORT\|RELAY_PORT' "$SERVER_UNIT")"; fi
    sed -i '/^Environment=NEODESK_SUPERVISOR=/a Environment=SIGNAL_PORT=31116\nEnvironment=RELAY_PORT=31117' "$SERVER_UNIT"
    printf 'SIGNAL_PORT=21116\nRELAY_PORT=21117\nSESSION_SECRET=kept\n' > "$CONSOLE_PATH/.env"
    maybe_update_services
    if grep -q 'SIGNAL_PORT\|RELAY_PORT' "$SERVER_UNIT"; then fail "the updated unit sets $(grep 'SIGNAL_PORT\|RELAY_PORT' "$SERVER_UNIT")"; fi
    grep -qx 'SIGNAL_PORT=31116' "$CONSOLE_PATH/.env" && grep -qx 'RELAY_PORT=31117' "$CONSOLE_PATH/.env" \
        || fail "the update lost the unit's ports: $(grep '_PORT=' "$CONSOLE_PATH/.env" | tr '\n' ' ')"
    grep -qx 'SESSION_SECRET=kept' "$CONSOLE_PATH/.env" || fail "the update lost a line of .env"

    # A repair puts the ports into .env when it has none, and leaves the
    # admin's alone
    ensure_go_server_signal_ports || true
    grep -qx 'SIGNAL_PORT=31116' "$CONSOLE_PATH/.env" || fail "a repair changed the admin's signal port"
    printf 'SESSION_SECRET=kept\n' > "$CONSOLE_PATH/.env"
    ensure_go_server_signal_ports || true
    grep -qx 'SIGNAL_PORT=21116' "$CONSOLE_PATH/.env" && grep -qx 'RELAY_PORT=21117' "$CONSOLE_PATH/.env" \
        || fail "a repair left .env without ports: $(cat "$CONSOLE_PATH/.env")"
    if grep -q 'SIGNAL_PORT\|RELAY_PORT' "$SERVER_UNIT"; then fail "a repair pinned $(grep 'SIGNAL_PORT\|RELAY_PORT' "$SERVER_UNIT")"; fi
    rm -rf "$TEST_DIR"
)

test_node_era_detection() (
    load
    write_node_era_units
    if is_go_only_install; then fail "a Node-era unit counts as the server with its console"; fi
    is_node_era_install || fail "is_node_era_install"
    rm -rf "$TEST_DIR"
)

# NeoDesk has no way from the old Node.js console: the options of the
# upgrade's rollback and clean-up are gone, and an install or an update on
# a server that still runs the Node.js console stops with what to do and
# changes nothing.
test_no_upgrade_from_the_node_console() (
    local opt out code
    for opt in --rollback-go --cleanup-node; do
        code=0
        out=$(bash "$SCRIPT_ROOT/install/neodesk.sh" "$opt" 2>&1 < /dev/null) || code=$?
        [ "$code" = 1 ] && [ "$out" = "Unknown option: $opt" ] || fail "$opt: exit $code: $out"
    done
    load
    write_node_era_units
    printf 'PORT=5000\nSESSION_SECRET=kept\n' > "$CONSOLE_PATH/.env"
    : > "$CONSOLE_PATH/server.js"
    print_header() { :; }
    print_error() { echo "$1"; }
    print_info() { echo "$1"; }
    detect_installation() { INSTALL_STATUS=partial; }
    press_enter() { :; }
    systemctl() { echo "systemctl $*" >> "$TEST_DIR/calls"; }
    SERVER_BINARY_FILE="$TEST_DIR/no-binary"
    local before
    before=$(cat "$SERVER_UNIT" "$LEGACY_CONSOLE_UNIT" "$CONSOLE_PATH/.env" | sha256sum)
    AUTO_MODE=true
    UPDATE_MODE=true
    for action in do_update do_install; do
        code=0
        out=$("$action" 2>&1) || code=$?
        [ "$code" != 0 ] || fail "$action on an install with the Node.js console reported success: $out"
        case "$out" in *"does not upgrade"*"install NeoDesk on a clean server"*) ;; *) fail "$action does not say what to do: $out" ;; esac
    done
    [ "$(cat "$SERVER_UNIT" "$LEGACY_CONSOLE_UNIT" "$CONSOLE_PATH/.env" | sha256sum)" = "$before" ] || fail "a refused install changed the units or .env"
    [ ! -e "$CONSOLE_PATH/.env.pre-go" ] && [ ! -e "$TEST_DIR/calls" ] || fail "a refused install started on the upgrade: $(cat "$TEST_DIR/calls" 2>/dev/null)"
    [ "$(auto_action)" = install ] || fail "a run without questions on it does $(auto_action)"
    rm -rf "$TEST_DIR"
)

# The update's way back: a new binary that does not come up is replaced by
# the one from before (kept as neodesk-server.prev), which starts again.
test_failed_update_puts_the_previous_binary_back() (
    load
    printf '#!/bin/sh\n# upgrade-env --from FILE; verify-release (the release before)\necho before\n' > "$RUSTDESK_PATH/neodesk-server"
    chmod +x "$RUSTDESK_PATH/neodesk-server"
    printf '#!/bin/sh\n# upgrade-env --from FILE; verify-release (a release that does not start)\necho broken\n' > "$TEST_DIR/broken"
    SERVER_BINARY_FILE="$TEST_DIR/broken"
    print_step() { :; }
    print_error() { echo "$1"; }
    print_success() { :; }
    print_warning() { :; }
    do_backup_silent() { :; }
    merge_console_env() { :; }
    maybe_update_services() { :; }
    set_server_folder_permissions() { :; }
    wait_for_service_stop() { :; }
    install_manager_command() { echo "the commands were installed" >> "$TEST_DIR/calls"; }
    systemctl() { echo "systemctl $*" >> "$TEST_DIR/calls"; }
    # The service comes up only with the binary from before
    verify_go_install() { [ "$("$RUSTDESK_PATH/neodesk-server")" = before ]; }
    local out code=0
    out=$(update_go_install 2>&1) || code=$?
    [ "$code" != 0 ] || fail "an update to a binary that does not come up reported success: $out"
    [ "$("$RUSTDESK_PATH/neodesk-server")" = before ] || fail "the binary from before is not back: $("$RUSTDESK_PATH/neodesk-server")"
    case "$out" in *"putting the previous one back"*) ;; *) fail "the update does not say it went back: $out" ;; esac
    grep -q '^systemctl start neodesk-server' "$TEST_DIR/calls" || fail "the service was not started again: $(cat "$TEST_DIR/calls")"
    if grep -q 'the commands were installed' "$TEST_DIR/calls"; then fail "a failed update replaced the installer's commands"; fi
    rm -rf "$TEST_DIR"
)

# Stopping stale servers ends a running neodesk-server, never a process
# that only names the binary in its arguments (the installer run with
# --binary /dist/neodesk-server-linux-amd64).
test_kill_stale_processes() (
    load
    print_warning() { :; }
    mkdir -p "$TEST_DIR/bin"
    cp "$(command -v sleep)" "$TEST_DIR/bin/neodesk-server"
    "$TEST_DIR/bin/neodesk-server" 60 &
    local server=$!
    bash -c 'sleep 60; : --binary /dist/neodesk-server-linux-amd64' &
    local installer=$!
    sleep 0.5
    kill_stale_processes "neodesk-server"
    if kill -0 "$server" 2>/dev/null; then
        kill "$server" "$installer" 2>/dev/null || true
        fail "the stale neodesk-server kept running"
    fi
    if ! kill -0 "$installer" 2>/dev/null; then
        fail "a process that only names neodesk-server in its arguments was killed"
    fi
    kill "$installer" 2>/dev/null || true
    wait 2>/dev/null || true
    rm -rf "$TEST_DIR"
)

# A fresh .env gives the server the admin password the console shows
# (INIT_ADMIN_PASS): the server makes the first admin before the console
# starts, and the console keeps an admin it did not make.
test_fresh_env_admin_password() (
    load
    cat > "$RUSTDESK_PATH/neodesk-server" <<'FAKE'
#!/bin/bash
# Stands in for the binary: upgrade-env --from FILE (the commands it has)
if [ "$1 $2" = "env merge" ]; then
    printf 'DEFAULT_ADMIN_PASSWORD=%s\nDB_TYPE=sqlite\n' "$ND_SUBST_DEFAULT_ADMIN_PASSWORD" > "$4"
    echo '{"success":true,"mode":"fresh","added":[]}'
fi
exit 0
FAKE
    chmod +x "$RUSTDESK_PATH/neodesk-server"
    ADMIN_PASSWORD='Fresh-Admin-Pass-1'
    USE_POSTGRESQL=false
    merge_console_env true >/dev/null
    grep -qx 'INIT_ADMIN_PASS=Fresh-Admin-Pass-1' "$CONSOLE_PATH/.env" \
        || fail "a fresh .env has no INIT_ADMIN_PASS of the admin password: $(grep -c INIT_ADMIN_PASS "$CONSOLE_PATH/.env") lines"
    # The binary stands in for an older one, which reads DB_URL
    grep -qx "DB_URL=$RUSTDESK_PATH/db_v2.sqlite3" "$CONSOLE_PATH/.env" || fail "DB_URL of a fresh SQLite install on an older binary"
    # L65: a fresh install starts in Managed; .env is only the first
    # default, a mode saved in the console wins
    grep -qx 'ENROLLMENT_MODE=managed' "$CONSOLE_PATH/.env" || fail "a fresh .env does not start in Managed"
    rm -rf "$TEST_DIR"
)

# database_lines FILE: the lines of an env file that name the database.
database_lines() {
    grep -E '^(DATABASE_URL|DB_URL|DB_TYPE|DB_PATH)=' "$1" | tr '\n' ' '
}

# L70: the installer writes the database once, as DATABASE_URL, for a
# binary that reads it; older env files are read as the server reads them.
test_database_url_only() (
    load
    cat > "$RUSTDESK_PATH/neodesk-server" <<'FAKE'
#!/bin/bash
# Stands in for the binary: upgrade-env --from FILE (the commands it has),
# and the server's warning "Remove DB_TYPE to use DATABASE_URL". env merge
# writes the template's lines, or appends those the file lacks
if [ "$1 $2" = "env merge" ]; then
    case " $* " in *" --fresh "*) : > "$4" ;; esac
    grep -q '^DEFAULT_ADMIN_PASSWORD=' "$4" 2>/dev/null || printf 'DEFAULT_ADMIN_PASSWORD=%s\n' "$ND_SUBST_DEFAULT_ADMIN_PASSWORD" >> "$4"
    grep -q '^DATABASE_URL=' "$4" 2>/dev/null || printf 'DATABASE_URL=%s\n' "$ND_SUBST_DATABASE_URL" >> "$4"
    echo '{"success":true,"mode":"merge","added":[]}'
fi
exit 0
FAKE
    chmod +x "$RUSTDESK_PATH/neodesk-server"
    ADMIN_PASSWORD='Database-Url-Pass-1'
    USE_POSTGRESQL=false
    merge_console_env true >/dev/null
    [ "$(database_lines "$CONSOLE_PATH/.env")" = "DATABASE_URL=$RUSTDESK_PATH/db_v2.sqlite3 " ] \
        || fail "a fresh SQLite install names its database as: $(database_lines "$CONSOLE_PATH/.env")"
    local pg='postgres://bd:p&w|d@127.0.0.1:5432/bd?sslmode=disable'
    rm -f "$CONSOLE_PATH/.env"
    USE_POSTGRESQL=true
    POSTGRESQL_URI="$pg"
    merge_console_env true >/dev/null
    [ "$(database_lines "$CONSOLE_PATH/.env")" = "DATABASE_URL=$pg " ] \
        || fail "a fresh PostgreSQL install names its database as: $(database_lines "$CONSOLE_PATH/.env")"

    # A move between databases leaves DATABASE_URL alone, also in an older
    # file (one without its last line end)
    printf 'SESSION_SECRET=kept\nDB_TYPE=sqlite\nDB_PATH=/old/db.sqlite3\nDATABASE_URL=\nDB_URL=/old/db.sqlite3' > "$CONSOLE_PATH/.env"
    use_database_in_env postgres "$pg"
    [ "$(database_lines "$CONSOLE_PATH/.env")" = "DATABASE_URL=$pg " ] \
        || fail "the move to PostgreSQL names the database as: $(database_lines "$CONSOLE_PATH/.env")"
    grep -qx 'SESSION_SECRET=kept' "$CONSOLE_PATH/.env" || fail "the move lost another line of .env"
    use_database_in_env sqlite /data/db_v2.sqlite3
    [ "$(database_lines "$CONSOLE_PATH/.env")" = "DATABASE_URL=/data/db_v2.sqlite3 " ] \
        || fail "the move back to SQLite names the database as: $(database_lines "$CONSOLE_PATH/.env")"

    # An update keeps an older file as it is, and preserves its database
    printf 'DB_TYPE=postgres\nDATABASE_URL=%s\nDB_URL=%s\n' "$pg" "$pg" > "$CONSOLE_PATH/.env"
    USE_POSTGRESQL=false
    POSTGRESQL_URI=""
    preserve_database_config
    [ "$USE_POSTGRESQL" = true ] && [ "$POSTGRESQL_URI" = "$pg" ] || fail "an older PostgreSQL install is not seen as one"
    merge_console_env false >/dev/null
    [ "$(database_lines "$CONSOLE_PATH/.env")" = "DB_TYPE=postgres DATABASE_URL=$pg DB_URL=$pg " ] \
        || fail "an update changed an older file's database: $(database_lines "$CONSOLE_PATH/.env")"
    # A file of this installer names PostgreSQL by its address alone
    printf 'DATABASE_URL=%s\n' "$pg" > "$CONSOLE_PATH/.env"
    USE_POSTGRESQL=false
    preserve_database_config
    [ "$USE_POSTGRESQL" = true ] && [ "$POSTGRESQL_URI" = "$pg" ] || fail "DATABASE_URL alone is not seen as PostgreSQL"
    printf 'SESSION_SECRET=kept\n' > "$CONSOLE_PATH/.env"
    USE_POSTGRESQL=false
    merge_console_env false >/dev/null
    [ "$(database_lines "$CONSOLE_PATH/.env")" = "DATABASE_URL=$RUSTDESK_PATH/db_v2.sqlite3 " ] \
        || fail "an update of a file that names no database writes: $(database_lines "$CONSOLE_PATH/.env")"

    # Read as the server reads them (envvar.DatabaseFrom)
    local file="$TEST_DIR/read.env" want text
    while IFS='|' read -r want text; do
        printf '%b' "$text" > "$file"
        [ "$(env_database_type "$file") $(env_database_url "$file")" = "$want" ] \
            || fail "$text is read as '$(env_database_type "$file") $(env_database_url "$file")', want '$want'"
    done <<'CASES'
postgres postgres://a@h/d|DB_TYPE=postgres\nDATABASE_URL=postgres://a@h/d\nDB_URL=postgres://a@h/d\n
sqlite /old/db|DB_TYPE=sqlite\nDATABASE_URL=\nDB_URL=/old/db\n
sqlite /old/db|DB_TYPE=sqlite\nDATABASE_URL=postgres://a@h/d\nDB_URL=/old/db\n
sqlite |DB_TYPE=SQLite\nDATABASE_URL=postgres://a@h/d\nDB_URL=postgresql://a@h/d\n
postgres postgresql://a@h/d|DATABASE_URL=postgresql://a@h/d\n
postgres postgres://a@h/d|DATABASE_URL=\nDB_URL=postgres://a@h/d\n
sqlite /srv/db.sqlite3|DATABASE_URL=sqlite:///srv/db.sqlite3\nDB_URL=/other\n
sqlite /srv/db.sqlite3|DATABASE_URL=SQLite:/srv/db.sqlite3\n
sqlite |SESSION_SECRET=x\n
CASES
    rm -rf "$TEST_DIR"
)

# UP8: an update channel given to the installer (install.sh passes its
# --channel) goes into .env over the template's main, on a fresh
# install and on an update; without one .env keeps what it has.
test_update_channel_in_env() (
    export UPDATE_GITHUB_BRANCH=dev
    load
    cat > "$RUSTDESK_PATH/neodesk-server" <<'FAKE'
#!/bin/bash
# Stands in for the binary: upgrade-env --from FILE (the commands it has)
if [ "$1 $2" = "env merge" ]; then
    grep -q '^UPDATE_GITHUB_BRANCH=' "$4" 2>/dev/null || printf 'UPDATE_GITHUB_BRANCH=main\n' >> "$4"
    echo '{"success":true,"mode":"merge","added":[]}'
fi
exit 0
FAKE
    chmod +x "$RUSTDESK_PATH/neodesk-server"
    ADMIN_PASSWORD='Channel-Test-1'
    USE_POSTGRESQL=false
    merge_console_env true >/dev/null
    grep -qx 'UPDATE_GITHUB_BRANCH=dev' "$CONSOLE_PATH/.env" \
        || fail "a fresh install from dev writes $(grep '^UPDATE_GITHUB_BRANCH=' "$CONSOLE_PATH/.env")"

    # An update reads the channel before it downloads the binary
    printf 'UPDATE_GITHUB_BRANCH=main\n' > "$CONSOLE_PATH/.env"
    read_update_github_branch_from_env
    [ "$UPDATE_GITHUB_BRANCH" = dev ] || fail "the given channel lost to the main in .env: $UPDATE_GITHUB_BRANCH"
    merge_console_env false >/dev/null
    [ "$(grep -c '^UPDATE_GITHUB_BRANCH=' "$CONSOLE_PATH/.env")" = 1 ] || fail "the key is in .env twice"
    grep -qx 'UPDATE_GITHUB_BRANCH=dev' "$CONSOLE_PATH/.env" \
        || fail "an update with a given channel keeps $(grep '^UPDATE_GITHUB_BRANCH=' "$CONSOLE_PATH/.env")"
    rm -rf "$TEST_DIR"
)

# The server's own updater reads the channel from .env, so a given channel
# is in .env before neodesk-server update runs
test_update_channel_before_server_update() (
    export UPDATE_GITHUB_BRANCH=dev
    load
    printf 'UPDATE_GITHUB_BRANCH=main\n' > "$CONSOLE_PATH/.env"
    do_backup_silent() { :; }
    print_step() { :; }
    print_error() { :; }
    SERVER_BINARY_FILE=""
    FROM_SOURCE=false
    unset RELEASE_BASE_URL
    fake_current_binary
    server_cmd() {
        grep '^UPDATE_GITHUB_BRANCH=' "$CONSOLE_PATH/.env" > "$TEST_DIR/seen"
        return 1
    }
    update_go_install >/dev/null 2>&1 || true
    [ "$(cat "$TEST_DIR/seen" 2>/dev/null)" = "UPDATE_GITHUB_BRANCH=dev" ] \
        || fail "neodesk-server update ran with $(cat "$TEST_DIR/seen" 2>/dev/null || echo 'no channel') in .env"
    rm -rf "$TEST_DIR"
)

test_update_channel_kept_without_one() (
    unset UPDATE_GITHUB_BRANCH
    load
    cat > "$RUSTDESK_PATH/neodesk-server" <<'FAKE'
#!/bin/bash
# Stands in for the binary: upgrade-env --from FILE (the commands it has)
[ "$1 $2" = "env merge" ] && echo '{"success":true,"mode":"merge","added":[]}'
exit 0
FAKE
    chmod +x "$RUSTDESK_PATH/neodesk-server"
    USE_POSTGRESQL=false
    printf 'UPDATE_GITHUB_BRANCH=dev\nDEFAULT_ADMIN_PASSWORD=x\n' > "$CONSOLE_PATH/.env"
    merge_console_env false >/dev/null
    grep -qx 'UPDATE_GITHUB_BRANCH=dev' "$CONSOLE_PATH/.env" || fail "an update without a channel changed .env"
    if grep -q '^ENROLLMENT_MODE' "$CONSOLE_PATH/.env"; then fail "an update added an enrollment mode to .env"; fi
    read_update_github_branch_from_env
    [ "$UPDATE_GITHUB_BRANCH" = dev ] || fail "the channel of .env was not read: $UPDATE_GITHUB_BRANCH"
    rm -rf "$TEST_DIR"
)

# The server folder holds the database and the keys: the service's group
# may write it, nobody else may list or read it (upstream audit C3).
test_server_folder_mode() (
    load
    SERVICE_USER=$(id -un)
    printf 'x' > "$RUSTDESK_PATH/db_v2.sqlite3"
    set_server_folder_permissions
    mode=$(stat -c %a "$RUSTDESK_PATH")
    [ "$mode" = 2770 ] || fail "the server folder has mode $mode, want 2770"
    rm -rf "$TEST_DIR"
)

# The console's language files, themes and images come from the binary:
# an update copies none from a web-nodejs folder an older release left in
# the checkout, and the console folder keeps its .env, data, installed
# fonts and language files.
test_console_home_copies_no_console_files() (
    load
    SCRIPT_DIR="$TEST_DIR/checkout"
    mkdir -p "$SCRIPT_DIR/web-nodejs/lang" "$SCRIPT_DIR/web-nodejs/themes"
    printf '{"old":true}\n' > "$SCRIPT_DIR/web-nodejs/lang/en.json"
    printf 'old\n' > "$SCRIPT_DIR/web-nodejs/themes/dark.css"
    mkdir -p "$CONSOLE_PATH/data/transfers" "$CONSOLE_PATH/public/fonts" "$CONSOLE_PATH/lang"
    printf 'SESSION_SECRET=kept\n' > "$CONSOLE_PATH/.env"
    printf 'file\n' > "$CONSOLE_PATH/data/transfers/a.bin"
    printf 'font\n' > "$CONSOLE_PATH/public/fonts/inter.woff2"
    printf '{"custom":true}\n' > "$CONSOLE_PATH/lang/xx.json"
    SERVICE_USER=$(id -un)
    MINIMAL_MODE=false
    STORE_ADMIN_CREDENTIALS=false
    print_step() { :; }
    print_success() { :; }
    ensure_service_user() { :; }
    merge_console_env() { [ "$1" = false ] || fail "an install with .env counted as fresh"; }
    setup_console_home >/dev/null
    [ ! -e "$CONSOLE_PATH/lang/en.json" ] || fail "the update copied the checkout's web-nodejs/lang into the console folder"
    [ ! -e "$CONSOLE_PATH/themes" ] || fail "the update copied the checkout's web-nodejs/themes into the console folder"
    [ "$(cat "$CONSOLE_PATH/.env")" = SESSION_SECRET=kept ] || fail "the update changed .env"
    [ -f "$CONSOLE_PATH/data/transfers/a.bin" ] || fail "the update lost a file of data/"
    [ -f "$CONSOLE_PATH/public/fonts/inter.woff2" ] || fail "the update lost an installed font"
    [ -f "$CONSOLE_PATH/lang/xx.json" ] || fail "the update lost a language file of the console folder"
    rm -rf "$TEST_DIR"
)

# L31: releases, the update channel and the unit's documentation link
# name the fork's repository.
test_releases_come_from_the_fork() (
    unset UPDATE_GITHUB_OWNER UPDATE_GITHUB_REPO UPDATE_GITHUB_BRANCH RELEASE_BASE_URL
    load
    local base
    base=$(release_base_url)
    [ "$base" = "https://github.com/neodeskapp/neodesk/releases/latest/download" ] \
        || fail "the stable channel downloads from $base"
    UPDATE_GITHUB_BRANCH=dev
    base=$(release_base_url)
    [ "$base" = "https://github.com/neodeskapp/neodesk/releases/download/dev" ] \
        || fail "the development channel downloads from $base"
    write_server_unit "-mode all" ""
    grep -qx "Documentation=https://github.com/neodeskapp/neodesk" "$SERVER_UNIT" \
        || fail "the unit's documentation link: $(grep '^Documentation=' "$SERVER_UNIT")"
    rm -rf "$TEST_DIR"
)

# L140: --channel picks the update channel, and a failed download says
# what to do: no stable release yet, or no way out to the internet.
test_channel_and_download_errors() (
    unset UPDATE_GITHUB_BRANCH RELEASE_BASE_URL UPDATE_GITHUB_OWNER UPDATE_GITHUB_REPO
    set -- --channel dev
    # shellcheck disable=SC1091
    source "$SCRIPT_ROOT/install/neodesk.sh"
    [ "$UPDATE_GITHUB_BRANCH" = dev ] && [ "$UPDATE_GITHUB_BRANCH_GIVEN" = dev ] \
        || fail "--channel dev gives the channel '$UPDATE_GITHUB_BRANCH' (given '$UPDATE_GITHUB_BRANCH_GIVEN')"
    load
    print_info() { echo "$1"; }
    unset UPDATE_GITHUB_BRANCH_GIVEN
    UPDATE_GITHUB_BRANCH=main
    local out
    curl() { printf '404'; }
    out=$(download_release_binary "$TEST_DIR" 2>&1) && fail "a download that found nothing succeeded"
    case "$out" in
        *"no stable release"*"--channel dev"*"--binary"*) ;;
        *) fail "a missing stable release says: $out" ;;
    esac
    curl() { printf '000'; return 6; }
    out=$(download_release_binary "$TEST_DIR" 2>&1) && fail "a download without network succeeded"
    case "$out" in
        *"Could not reach"*"--binary"*) ;;
        *) fail "a download without network says: $out" ;;
    esac
    if bash "$SCRIPT_ROOT/install/neodesk.sh" --channel nightly >/dev/null 2>&1; then fail "--channel nightly was taken"; fi
    rm -rf "$TEST_DIR"
)

# L141: the installer runs piped from curl, takes no answers from a pipe,
# and leaves the neodesk command behind for updates and the uninstall.
test_one_command_install() (
    local out
    # main runs: it asks for root, or as root it looks for an install
    out=$(cd "$(mktemp -d)" && timeout 60 bash -s -- --check-permissions < "$SCRIPT_ROOT/install/neodesk.sh" 2>&1 || true)
    case "$out" in
        *"requires root"*|*"Detecting installation"*) ;;
        *) fail "neodesk.sh piped into bash did not start: $out" ;;
    esac
    load
    AUTO_MODE=false
    default_to_auto_without_terminal < /dev/null
    [ "$AUTO_MODE" = true ] || fail "without a terminal the installer would ask questions"
    out=$( (press_enter; confirm "Go on?" && echo yes || echo no) < /dev/null 2>/dev/null)
    case "$out" in
        *no) ;;
        *) fail "without a terminal a question did not take its default: $out" ;;
    esac
    commands_in_test_dir
    install_manager_command >/dev/null
    [ -x "$MANAGER_COMMAND" ] || fail "no neodesk-setup command was left behind"
    cmp -s "$MANAGER_COMMAND" "$SCRIPT_ROOT/install/neodesk.sh" || fail "the neodesk-setup command is not this installer"
    rm -rf "$TEST_DIR"
)

# A250: the console's default port is 3375. An install whose settings name
# a port (CONSOLE_PORT or PORT in .env or the unit) keeps it; one that
# names none followed the old default, 5000, and moves to 3375 at its
# update: the update says so at the end, with how to keep 5000, and opens
# 3375 in the firewall the installer manages.
test_update_moves_the_default_console_port() (
    load
    printf 'SESSION_SECRET=kept\n' > "$CONSOLE_PATH/.env"
    : > "$SERVER_UNIT"
    console_port_named && fail "an install without a port counts as one that names one"
    [ "$(resolve_panel_http_port)" = 3375 ] || fail "the default console port is $(resolve_panel_http_port)"
    configure_firewall_rules() { echo "firewall $(firewall_ports | tr '\n' ' ')" >> "$TEST_DIR/calls"; }
    local out
    out=$(note_console_port_move false 2>&1)
    case "$out" in
        *"The console now answers on port 3375; it was 5000"*"CONSOLE_PORT=5000"*"$CONSOLE_PATH/.env"*) ;;
        *) fail "the update does not say the console moved: $out" ;;
    esac
    grep -q 'firewall .*3375/tcp' "$TEST_DIR/calls" || fail "the update did not open 3375 in the firewall: $(cat "$TEST_DIR/calls" 2>&1)"
    : > "$TEST_DIR/calls"
    out=$(note_console_port_move true 2>&1)
    [ -z "$out" ] || fail "an install that names its port was told it moved: $out"
    [ ! -s "$TEST_DIR/calls" ] || fail "an install that names its port got firewall changes"
    for setting in 'CONSOLE_PORT=5000' 'CONSOLE_PORT=5080' 'PORT=5080'; do
        printf '%s\n' "$setting" > "$CONSOLE_PATH/.env"
        console_port_named || fail "$setting in .env does not count as a port the install names"
    done
    [ "$(resolve_panel_http_port)" = 5080 ] || fail "a configured port does not win: $(resolve_panel_http_port)"
    printf 'SESSION_SECRET=kept\n' > "$CONSOLE_PATH/.env"
    printf '[Service]\nEnvironment=CONSOLE_PORT=5000\n' > "$SERVER_UNIT"
    console_port_named || fail "CONSOLE_PORT in the unit does not count as a port the install names"
    rm -rf "$TEST_DIR"
)

# Piped from curl, and as the installed neodesk-setup, the installer takes
# neodesk.sh from the release it installs, never from a branch's raw
# files: the release folder this run downloaded from, else the release of
# the installed binary's version, or the "dev" release on the development
# channel. The release's SHA256SUMS and its signature (SHA256SUMS.sig)
# check the file; one that fails, or a release without neodesk.sh
# (3.6.0-rc.4 and older), leaves neodesk-setup as it is.
test_installer_comes_from_the_release() (
    load
    if ! command -v openssl >/dev/null || ! openssl pkeyutl -help 2>&1 | grep -q -- -rawin; then
        echo "skipped: no OpenSSL 3"
        exit 0
    fi
    commands_in_test_dir
    mkdir -p "$TEST_DIR/sbin" "$TEST_DIR/bin"
    openssl genpkey -algorithm ed25519 -out "$TEST_DIR/key.pem" 2>/dev/null
    openssl genpkey -algorithm ed25519 -out "$TEST_DIR/other.pem" 2>/dev/null
    RELEASE_PUBLIC_KEY_PEM=$(openssl pkey -in "$TEST_DIR/key.pem" -pubout 2>/dev/null)
    local rel="$TEST_DIR/release"
    mkdir -p "$rel"
    { cat "$SCRIPT_ROOT/install/neodesk.sh"; echo "# the release's copy"; } > "$rel/neodesk.sh"
    # sign_list KEY: the release's SHA256SUMS and its signature by KEY
    sign_list() {
        (cd "$rel" && sha256sum neodesk.sh > SHA256SUMS)
        { printf 'neodesk-release-files-v1\n'; cat "$rel/SHA256SUMS"; } > "$TEST_DIR/payload"
        openssl pkeyutl -sign -inkey "$1" -rawin -in "$TEST_DIR/payload" | base64 -w0 > "$rel/SHA256SUMS.sig"
    }
    sign_list "$TEST_DIR/key.pem"
    printf '#!/bin/sh\n[ "$1" = -version ] && echo "neodesk-server 3.6.0-rc.5 (built 2026-11-25T10:00:00Z, commit abc1234)"\nexit 0\n' \
        > "$RUSTDESK_PATH/neodesk-server"
    chmod +x "$RUSTDESK_PATH/neodesk-server"
    # curl answers from the release folder by the file's name
    curl() {
        local a prev="" dest="" url=""
        for a in "$@"; do [ "$prev" = -o ] && dest="$a"; prev="$a"; url="$a"; done
        printf '%s\n' "$url" >> "$TEST_DIR/fetched"
        if [ -f "$rel/${url##*/}" ]; then cp "$rel/${url##*/}" "$dest"; printf 200; else printf 404; fi
    }
    # from URL WHAT: piped from curl, neodesk-setup is the release's
    # neodesk.sh, fetched from URL
    from() {
        : > "$TEST_DIR/fetched"
        rm -f "$MANAGER_COMMAND"
        install_manager_command >/dev/null 2>&1
        grep -qxF -- "$1/neodesk.sh" "$TEST_DIR/fetched" || fail "$2: neodesk.sh came from $(tr '\n' ' ' < "$TEST_DIR/fetched")"
        cmp -s "$MANAGER_COMMAND" "$rel/neodesk.sh" || fail "$2: neodesk-setup is not the release's neodesk.sh"
        if grep -q githubusercontent "$TEST_DIR/fetched"; then fail "$2: a raw file was fetched"; fi
    }
    NEODESK_SELF=""
    UPDATE_GITHUB_BRANCH=main
    from https://github.com/neodeskapp/neodesk/releases/download/v3.6.0-rc.5 "the installed binary's release"
    UPDATE_GITHUB_BRANCH=dev
    from https://github.com/neodeskapp/neodesk/releases/download/dev "the development channel"
    RELEASE_FOLDER_USED=https://example.test/releases/x
    from https://example.test/releases/x "the folder this run installed from"
    unset RELEASE_FOLDER_USED
    UPDATE_GITHUB_BRANCH=main

    # kept WHAT: as the installed neodesk-setup, it stays as it is
    NEODESK_SELF="$MANAGER_COMMAND"
    kept() {
        install -m 0755 "$SCRIPT_ROOT/install/neodesk.sh" "$MANAGER_COMMAND"
        install_manager_command >/dev/null 2>&1
        cmp -s "$MANAGER_COMMAND" "$SCRIPT_ROOT/install/neodesk.sh" || fail "$1 replaced neodesk-setup"
    }
    printf '# changed\n' >> "$rel/neodesk.sh"
    kept "a neodesk.sh the release's SHA256SUMS does not name"
    sign_list "$TEST_DIR/other.pem"
    kept "a neodesk.sh whose list another key signed"
    rm -f "$rel/SHA256SUMS.sig"
    kept "a neodesk.sh without the list's signature"
    sign_list "$TEST_DIR/key.pem"
    install -m 0755 "$SCRIPT_ROOT/install/neodesk.sh" "$MANAGER_COMMAND"
    install_manager_command >/dev/null 2>&1
    cmp -s "$MANAGER_COMMAND" "$rel/neodesk.sh" || fail "the installed neodesk-setup did not update itself from the release"
    rm -f "$rel/neodesk.sh" "$rel/SHA256SUMS" "$rel/SHA256SUMS.sig"
    kept "a release without neodesk.sh"
    unset -f curl
    rm -rf "$TEST_DIR"
)

# L431: the installer's manager is neodesk-setup, and neodesk is the
# command line: a copy of the server binary (mode 755, root's own), which
# any user may run while the server folder stays closed, and which hands
# the installer's options to neodesk-setup. Nothing stays at the manager's
# old place, where it would come first in root's PATH.
test_install_places_neodesk_setup_and_the_command_line() (
    load
    [ "$MANAGER_COMMAND" = /usr/local/sbin/neodesk-setup ] || fail "the manager is installed as $MANAGER_COMMAND"
    [ "${CLI_COMMAND:-}" = /usr/local/bin/neodesk ] || fail "the command line is not /usr/local/bin/neodesk: ${CLI_COMMAND:-none}"
    [ "${OLD_MANAGER_COMMAND:-}" = /usr/local/sbin/neodesk ] || fail "the manager's old place is ${OLD_MANAGER_COMMAND:-unknown}"
    commands_in_test_dir
    fake_handing_binary
    install_manager_command >/dev/null
    [ -x "$MANAGER_COMMAND" ] || fail "no neodesk-setup"
    cmp -s "$MANAGER_COMMAND" "$SCRIPT_ROOT/install/neodesk.sh" || fail "neodesk-setup is not this installer"
    [ -f "$CLI_COMMAND" ] && [ ! -L "$CLI_COMMAND" ] && cmp -s "$CLI_COMMAND" "$RUSTDESK_PATH/neodesk-server" \
        || fail "neodesk is not a copy of the server binary: $(ls -l "$CLI_COMMAND" 2>&1)"
    [ "$(stat -c %a "$CLI_COMMAND")" = 755 ] || fail "neodesk has mode $(stat -c %a "$CLI_COMMAND")"
    [ ! -e "$OLD_MANAGER_COMMAND" ] || fail "something is at the manager's old place"
    local out
    out=$(print_install_summary 2>&1)
    for want in "sudo neodesk-setup --update" "sudo neodesk-setup --uninstall" "neodesk --help"; do
        case "$out" in *"$want"*) ;; *) fail "the summary does not name $want: $out" ;; esac
    done
    rm -rf "$TEST_DIR"
)

# L431: an install whose /usr/local/sbin/neodesk is the old manager
# (3.6.0-rc.4 or older) moves to neodesk-setup and the command line neodesk
# at its update, and so does one whose neodesk is the link into the server
# folder that builds before 3.6.0-rc.5 placed. Each update copies the new
# binary to neodesk.
# Until the server binary hands the installer's options over, neodesk
# stays the manager; a neodesk of someone else stays, and so does the old
# manager then. neodesk-setup is replaced by a new file, never written
# over: a copy that runs reads on from the file it opened.
test_update_moves_the_old_manager() (
    load
    commands_in_test_dir
    mkdir -p "$TEST_DIR/sbin"
    install -m 0755 "$SCRIPT_ROOT/install/neodesk.sh" "$OLD_MANAGER_COMMAND"
    fake_current_binary
    install_manager_command >/dev/null 2>&1
    [ -x "$MANAGER_COMMAND" ] || fail "no neodesk-setup next to an older binary"
    [ -x "$OLD_MANAGER_COMMAND" ] || fail "next to an older binary, neodesk is no longer the manager"
    [ ! -e "$CLI_COMMAND" ] && [ ! -L "$CLI_COMMAND" ] || fail "neodesk links to a binary that takes --update for a command of its own"

    fake_handing_binary
    install_manager_command >/dev/null 2>&1
    [ ! -L "$CLI_COMMAND" ] && cmp -s "$CLI_COMMAND" "$RUSTDESK_PATH/neodesk-server" || fail "the update placed no copy of the binary as neodesk"
    [ ! -e "$OLD_MANAGER_COMMAND" ] || fail "the old manager is still at /usr/local/sbin/neodesk"
    printf '# the next release\n' >> "$RUSTDESK_PATH/neodesk-server"
    install_manager_command >/dev/null 2>&1
    cmp -s "$CLI_COMMAND" "$RUSTDESK_PATH/neodesk-server" || fail "the update did not copy the new binary to neodesk"
    rm -f "$CLI_COMMAND"
    ln -s "$RUSTDESK_PATH/neodesk-server" "$CLI_COMMAND"
    install_manager_command >/dev/null 2>&1
    [ ! -L "$CLI_COMMAND" ] && cmp -s "$CLI_COMMAND" "$RUSTDESK_PATH/neodesk-server" \
        || fail "the link of an earlier build did not become the copy: $(ls -l "$CLI_COMMAND" 2>&1)"
    local inode
    inode=$(stat -c %i "$MANAGER_COMMAND")
    install_manager_command >/dev/null 2>&1
    [ "$(stat -c %i "$MANAGER_COMMAND")" != "$inode" ] || fail "neodesk-setup was written over in place"

    rm -f "$CLI_COMMAND"
    printf '#!/bin/sh\necho mine\n' > "$CLI_COMMAND"
    chmod 755 "$CLI_COMMAND"
    install -m 0755 "$SCRIPT_ROOT/install/neodesk.sh" "$OLD_MANAGER_COMMAND"
    local out
    out=$(install_manager_command 2>&1)
    grep -qx 'echo mine' "$CLI_COMMAND" || fail "a neodesk of someone else was replaced"
    [ -x "$OLD_MANAGER_COMMAND" ] || fail "the old manager went although neodesk is not the command line"
    case "$out" in *"$CLI_COMMAND"*) ;; *) fail "no word about the neodesk that stays: $out" ;; esac
    rm -rf "$TEST_DIR"
)

# L446: the installer's backups go to /opt/neodesk-backups. The backups of
# an install from before (in /opt/rustdesk-backups) move there at the
# installer's next run, and nothing is left at the old place, also not the
# link an earlier build left there. A BACKUP_DIR given stays as it is.
test_backups_move_to_the_neodesk_folder() (
    load
    [ "$BACKUP_DIR" = /opt/neodesk-backups ] || fail "the backups go to $BACKUP_DIR"
    [ "${OLD_BACKUP_DIR:-}" = /opt/rustdesk-backups ] || fail "the old backup folder is ${OLD_BACKUP_DIR:-unknown}"
    BACKUP_DIR="$TEST_DIR/neodesk-backups"
    OLD_BACKUP_DIR="$TEST_DIR/rustdesk-backups"
    mkdir -p "$OLD_BACKUP_DIR/pre-go-20260101-000000/units"
    printf 'old\n' > "$OLD_BACKUP_DIR/neodesk_backup_20260101_000000.tar.gz"
    chmod 700 "$OLD_BACKUP_DIR"
    move_backup_folder >/dev/null
    [ "$(cat "$BACKUP_DIR/neodesk_backup_20260101_000000.tar.gz" 2>/dev/null)" = old ] || fail "the old backup did not move"
    [ -d "$BACKUP_DIR/pre-go-20260101-000000/units" ] || fail "the upgrade's backup did not move"
    [ ! -e "$OLD_BACKUP_DIR" ] && [ ! -L "$OLD_BACKUP_DIR" ] || fail "something is left at the old place: $(ls -ld "$OLD_BACKUP_DIR" 2>&1)"
    [ "$(stat -c %a "$BACKUP_DIR")" = 700 ] || fail "the new folder has mode $(stat -c %a "$BACKUP_DIR"), the old one had 700"
    move_backup_folder >/dev/null || fail "a second run failed"
    [ ! -e "$OLD_BACKUP_DIR" ] && [ ! -L "$OLD_BACKUP_DIR" ] || fail "a second run left something at the old place"

    # The link an earlier build left at the old place goes; another link stays
    ln -s "$BACKUP_DIR" "$OLD_BACKUP_DIR"
    move_backup_folder >/dev/null || fail "a run with the earlier build's link failed"
    [ ! -L "$OLD_BACKUP_DIR" ] || fail "the earlier build's link at the old place stayed"
    [ -f "$BACKUP_DIR/neodesk_backup_20260101_000000.tar.gz" ] || fail "the backups went with the link"
    ln -s "$TEST_DIR/elsewhere" "$OLD_BACKUP_DIR"
    move_backup_folder >/dev/null || fail "a run with a link of someone else failed"
    [ "$(readlink "$OLD_BACKUP_DIR")" = "$TEST_DIR/elsewhere" ] || fail "a link that leads elsewhere was removed"
    rm -f "$OLD_BACKUP_DIR"

    # Both folders there: the old one's backups join the new ones, a name
    # in both stays in the old folder
    mkdir -p "$OLD_BACKUP_DIR"
    printf 'second\n' > "$OLD_BACKUP_DIR/neodesk_backup_20260202_000000.tar.gz"
    printf 'clash\n' > "$OLD_BACKUP_DIR/neodesk_backup_20260101_000000.tar.gz"
    move_backup_folder >/dev/null 2>&1
    [ -f "$BACKUP_DIR/neodesk_backup_20260202_000000.tar.gz" ] || fail "a backup of the old folder did not join the new one"
    [ ! -L "$OLD_BACKUP_DIR" ] || fail "the old folder became a link"
    [ "$(cat "$BACKUP_DIR/neodesk_backup_20260101_000000.tar.gz")" = old ] || fail "a backup of the new folder was written over"
    [ "$(cat "$OLD_BACKUP_DIR/neodesk_backup_20260101_000000.tar.gz" 2>/dev/null)" = clash ] || fail "a backup whose name is taken is gone"

    # A BACKUP_DIR given to the installer is used as it is
    local given="$TEST_DIR/given"
    rm -rf "$OLD_BACKUP_DIR" && mkdir -p "$OLD_BACKUP_DIR"
    BACKUP_DIR_GIVEN="$given" BACKUP_DIR="$given" move_backup_folder >/dev/null
    [ -d "$OLD_BACKUP_DIR" ] && [ ! -L "$OLD_BACKUP_DIR" ] && [ ! -e "$given" ] || fail "a given BACKUP_DIR moved the old folder"
    rm -rf "$TEST_DIR"
)

# The installers write no settings into the service of the old Node.js
# console (neodesk-console.service, NeoDeskConsole on Windows): no install
# makes it, and an install that still has it is not upgraded. Its unit is
# only read to recognise such an install and removed at the uninstall.
test_no_settings_go_to_the_old_console_service() {
    local hits
    hits=$(grep -nE '_(upsert|remove)_systemd_env "\$svc_file"|grep .*NEODESK_API_URL=.*neodesk-console\.service|local svc_file="/etc/systemd/system/neodesk-console\.service"' \
        "$SCRIPT_ROOT/install/neodesk.sh" || true)
    [ -z "$hits" ] || fail "neodesk.sh still writes or reads settings of neodesk-console.service:
$hits"
    hits=$(grep -nE '\$svcName = \$script:CONSOLE_SERVICE|"NeoDeskConsole" \}' "$SCRIPT_ROOT/install/neodesk.ps1" || true)
    [ -z "$hits" ] || fail "neodesk.ps1 still writes settings into the NeoDeskConsole service:
$hits"
}

# L201: a run without a terminal or TERM (cloud-init, CI, ssh host cmd)
# gets past the header instead of stopping at clear under set -e.
test_runs_without_term() (
    load
    local out
    # A shell of its own: set -e is in force there as in a run of the script
    out=$(env -u TERM bash -c 'script=$1; set --; source "$script"; print_header; echo "after the header"' \
        test "$SCRIPT_ROOT/install/neodesk.sh" 2>&1 < /dev/null) || true
    case "$out" in *"after the header"*) ;; *) fail "the header stopped a run without TERM: $out" ;; esac
    rm -rf "$TEST_DIR"
)

# L207: a package source that fails in apt-get update (a third-party
# repository, a mirror in sync) does not stop the install: the packages are
# installed from the lists there are, and the run says so.
test_apt_source_failure_goes_on() (
    local out log
    log=$(mktemp)
    out=$(bash -c 'script=$1 log=$2; set --; source "$script"
        LOG_FILE=$log
        apt-get() { if [ "$1" = update ]; then echo "E: Failed to fetch https://packages.example.org/Packages.xz" >&2; return 100; fi; echo "apt-get $* ($DEBIAN_FRONTEND, needrestart $NEEDRESTART_MODE)"; }
        install_dependencies; echo "after the dependencies"' test "$SCRIPT_ROOT/install/neodesk.sh" "$log" 2>&1 < /dev/null) || true
    case "$out" in *"after the dependencies"*) ;; *) fail "a failing package source stopped the install: $out" ;; esac
    case "$out" in *"apt-get update failed for a package source"*) ;; *) fail "the failing package source is not said: $out" ;; esac
    # LINUX-NATIVE-9: apt's output goes to the log, without questions, without
    # upgrading what is installed and without restarting other services
    case "$out" in *"Failed to fetch"*|*"apt-get install"*) fail "apt's output reached the screen: $out" ;; esac
    grep -q 'apt-get install --no-upgrade -y -qq sqlite3 .* (noninteractive, needrestart l)' "$log" \
        || fail "apt-get did not install quietly: $(cat "$log")"
    rm -f "$log"
)

# LINUX-NATIVE-15: --set-config names the keys that changed and restarts
# the server once; a value that is set already restarts nothing, and a
# refusal shows the server's reason.
test_set_config_says_what_changed() (
    load
    print_step() { :; }
    print_success() { echo "$1"; }
    print_error() { echo "$1"; }
    systemctl() { echo "systemctl $*" >> "$TEST_DIR/calls"; }
    start_services_with_verification() { echo "restart" >> "$TEST_DIR/calls"; }
    server_cmd() {
        case "$*" in
            *LOG_LEVEL=info*) printf 'LOG_LEVEL=info\nTRUST_PROXY=Y\n' >> "$CONSOLE_PATH/.env"
                printf '{\n  "envPath": "/x/.env",\n  "backupPath": "/x/.env.bak",\n  "changed": [\n    "LOG_LEVEL",\n    "TRUST_PROXY"\n  ],\n  "values": {}\n}\n' ;;
            # The server lists every key it set, also one that had the value
            *LOG_LEVEL=same*) printf '{\n  "envPath": "/x/.env",\n  "changed": [\n    "LOG_LEVEL"\n  ],\n  "values": {}\n}\n' ;;
            *) echo "FOO is not a setting this command changes" >&2; return 1 ;;
        esac
    }
    local out
    printf 'SESSION_SECRET=kept\n' > "$CONSOLE_PATH/.env"
    MANAGED_CONFIG_CHANGES=(LOG_LEVEL=info TRUST_PROXY=Y)
    out=$(apply_managed_config 2>&1) || fail "the change failed: $out"
    case "$out" in *"Changed in $CONSOLE_PATH/.env: LOG_LEVEL, TRUST_PROXY"*) ;; *) fail "the run does not name the changed keys: $out" ;; esac
    case "$out" in *envPath*|*backupPath*) fail "the server's report reached the screen: $out" ;; esac
    [ "$(grep -c '^restart$' "$TEST_DIR/calls")" = 1 ] || fail "not one restart: $(cat "$TEST_DIR/calls")"
    if grep -q 'systemctl restart' "$TEST_DIR/calls"; then fail "the server was restarted twice: $(cat "$TEST_DIR/calls")"; fi

    : > "$TEST_DIR/calls"
    MANAGED_CONFIG_CHANGES=(LOG_LEVEL=same)
    out=$(apply_managed_config 2>&1) || fail "a change to the same value failed: $out"
    case "$out" in *"Nothing changed"*) ;; *) fail "a change to the same value says: $out" ;; esac
    if grep -q '^restart$' "$TEST_DIR/calls"; then fail "a change to the same value restarted the server"; fi

    MANAGED_CONFIG_CHANGES=(FOO=bar)
    out=$(apply_managed_config 2>&1) && fail "a refused change reported success"
    case "$out" in *"FOO is not a setting"*) ;; *) fail "the refusal does not give the reason: $out" ;; esac
    rm -rf "$TEST_DIR"
)

# --reset-two-factor: --reset-password passes it to the server's
# reset-password when the binary has it, says that two-factor sign-in is
# off, and refuses with an update hint when the binary is older. Without it
# the reset says how to turn two-factor sign-in off too. --help names it,
# and alone it is a password reset too.
test_reset_password_can_reset_two_factor() (
    load
    print_header() { :; }
    print_error() { echo "$1"; }
    print_info() { echo "$1"; }
    auto_detect_paths() { CONSOLE_TYPE=go; }
    forget_first_admin_password() { :; }
    server_cmd() { cat >/dev/null; echo "$*" > "$TEST_DIR/args"; echo "Password updated for admin"; }
    AUTO_MODE=true
    ADMIN_PASSWORD=Reset-Pass-123
    printf '#!/bin/sh\n# reset-password [--reset-two-factor] [--verbose] [username]\n' > "$RUSTDESK_PATH/neodesk-server"
    chmod +x "$RUSTDESK_PATH/neodesk-server"
    local out
    RESET_TWO_FACTOR=true
    out=$(do_reset_password 2>&1) || fail "the reset with --reset-two-factor failed: $out"
    [ "$(cat "$TEST_DIR/args")" = "--as-service reset-password --reset-two-factor admin" ] \
        || fail "the server's reset ran as: $(cat "$TEST_DIR/args")"
    case "$out" in *"Two-factor sign-in is off for admin"*) ;; *) fail "the reset does not say two-factor sign-in is off: $out" ;; esac

    RESET_TWO_FACTOR=false
    out=$(do_reset_password 2>&1) || fail "the reset failed: $out"
    [ "$(cat "$TEST_DIR/args")" = "--as-service reset-password admin" ] || fail "a plain reset ran as: $(cat "$TEST_DIR/args")"
    case "$out" in *"--reset-password --reset-two-factor"*) ;; *) fail "a plain reset does not name --reset-two-factor: $out" ;; esac

    # An older binary does not have it
    printf '#!/bin/sh\n# reset-password [username]\n' > "$RUSTDESK_PATH/neodesk-server"
    rm -f "$TEST_DIR/args"
    RESET_TWO_FACTOR=true
    out=$(do_reset_password 2>&1) && fail "an older binary was asked to reset two-factor sign-in: $out"
    [ ! -e "$TEST_DIR/args" ] || fail "the reset ran on an older binary: $(cat "$TEST_DIR/args")"
    case "$out" in *"--update"*) ;; *) fail "the refusal does not say to update: $out" ;; esac
    rm -rf "$TEST_DIR"

    out=$(bash "$SCRIPT_ROOT/install/neodesk.sh" --help 2>&1)
    case "$out" in *"--reset-two-factor"*) ;; *) fail "--help does not name --reset-two-factor" ;; esac
    out=$(bash -c 'script=$1; source "$script" --reset-two-factor; echo "$RESET_PASSWORD_MODE $RESET_TWO_FACTOR"' test "$SCRIPT_ROOT/install/neodesk.sh" 2>&1 < /dev/null)
    [ "$out" = "true true" ] || fail "--reset-two-factor alone: $out"
)

# LINUX-NATIVE-10: --protocol did nothing; it is no longer in --help and
# says so instead of being taken silently.
test_protocol_flag_says_it_does_nothing() (
    local out
    out=$(bash "$SCRIPT_ROOT/install/neodesk.sh" --protocol https --help 2>&1) || fail "--protocol stopped the run: $out"
    case "$out" in *"--protocol does nothing"*) ;; *) fail "--protocol is taken silently: $out" ;; esac
    case "$out" in *"--protocol MODE"*) fail "--help still offers --protocol: $out" ;; esac
)

# LINUX-NATIVE-18: the installer's screens use NeoDesk's names, and the
# password reset shows its own result, not the server's details.
test_installer_uses_neodesk_names() (
    local hits
    hits=$(grep -nE 'HBBS \(Signal\)|HBBR \(Relay\)|RustDesk path|Detected RustDesk installation|RustDesk directory|Local Flutter/Cargo|Option 5 in SSL menu|^ *echo -e "  RustDesk:' \
        "$SCRIPT_ROOT/install/neodesk.sh" || true)
    [ -z "$hits" ] || fail "neodesk.sh still shows old names:
$hits"
    load
    print_header() { :; }
    auto_detect_paths() { CONSOLE_TYPE=go; }
    forget_first_admin_password() { :; }
    server_cmd() { cat >/dev/null; printf 'Auth database (SQLite): /x\nHash type: pbkdf2-sha256, length: 118\nDone\n'; }
    AUTO_MODE=true
    ADMIN_PASSWORD=Reset-Pass-123
    local out
    out=$(do_reset_password 2>&1) || fail "the reset failed: $out"
    case "$out" in *Reset-Pass-123*) ;; *) fail "the reset does not show the new password: $out" ;; esac
    case "$out" in *"Hash type"*|*"Auth database"*) fail "the reset shows the server's details: $out" ;; esac
    rm -rf "$TEST_DIR"
)

# L73: the installers' backup is the server's own (a consistent database
# copy, PostgreSQL too, the env file, the console's data and certificates)
# plus the server's keys; a binary without the backup command, or one whose
# backup fails, gets a copy of the SQLite file as before.
test_backup_uses_the_servers_backup() (
    load
    print_info() { echo "$1"; }
    print_warning() { echo "$1"; }
    print_success() { echo "$1"; }
    BACKUP_DIR="$TEST_DIR/backups"
    DB_PATH="$RUSTDESK_PATH/db_v2.sqlite3"
    printf 'sqlite file\n' > "$DB_PATH"
    printf 'private key\n' > "$RUSTDESK_PATH/id_ed25519"
    printf 'SESSION_SECRET=kept\n' > "$CONSOLE_PATH/.env"
    cat > "$RUSTDESK_PATH/neodesk-server" <<FAKE
#!/bin/bash
# Stands in for the binary: upgrade-env --from FILE, backup [--out DIR] (its commands)
printf '%s|%s\n' "\$*" "\$NEODESK_ENV_FILE" > "$TEST_DIR/args"
[ -f "$TEST_DIR/backup-fails" ] && { echo "open PostgreSQL: refused" >&2; exit 1; }
printf 'archive\n' > "\$3/neodesk-full-backup.zip"
echo "\$3/neodesk-full-backup.zip"
FAKE
    chmod +x "$RUSTDESK_PATH/neodesk-server"
    local out list
    out=$(do_backup_silent 2>&1) || fail "the backup failed: $out"
    [ "$(cut -d'|' -f2 "$TEST_DIR/args")" = "$CONSOLE_PATH/.env" ] || fail "the server's backup did not get the env file: $(cat "$TEST_DIR/args")"
    case "$(cut -d'|' -f1 "$TEST_DIR/args")" in "backup --out $BACKUP_DIR/neodesk_backup_"*) ;; *) fail "the server's backup ran as: $(cat "$TEST_DIR/args")" ;; esac
    list=$(tar -tzf "$BACKUP_DIR"/neodesk_backup_*.tar.gz)
    case "$list" in *neodesk-full-backup.zip*) ;; *) fail "the backup has no archive of the server: $list" ;; esac
    case "$list" in *id_ed25519*) ;; *) fail "the backup has no server key: $list" ;; esac
    case "$list" in *db_v2.sqlite3*) fail "the backup copied the live SQLite file beside the server's backup: $list" ;; esac

    rm -f "$BACKUP_DIR"/*.tar.gz
    touch "$TEST_DIR/backup-fails"
    out=$(do_backup_silent 2>&1) || fail "the backup with a failing server backup failed: $out"
    case "$out" in *"neodesk-server backup failed"*) ;; *) fail "the failed server backup is not said: $out" ;; esac
    # Listed to a file first: grep -q stops at its first match, and with
    # pipefail tar's broken pipe would fail the check
    tar -tzf "$BACKUP_DIR"/neodesk_backup_*.tar.gz > "$TEST_DIR/backup.list" || fail "the backup can't be listed"
    grep -q 'db_v2.sqlite3$' "$TEST_DIR/backup.list" || fail "without the server's backup the database file is not in the backup"
    rm -rf "$TEST_DIR"
)

# LINUX-NATIVE-1: the installer's backups hold the server's private key and
# every secret of .env, so only root reads them: the folder is 0700, each
# backup 0600, also inside the archive, and backups an older installer left
# readable are closed at the next run. A BACKUP_DIR given keeps its mode.
test_backups_are_for_root_only() (
    load
    print_step() { :; }
    print_success() { :; }
    print_warning() { :; }
    BACKUP_DIR="$TEST_DIR/backups"
    DB_PATH="$RUSTDESK_PATH/db_v2.sqlite3"
    printf 'sqlite file\n' > "$DB_PATH"
    printf 'private key\n' > "$RUSTDESK_PATH/id_ed25519"
    printf 'api key\n' > "$RUSTDESK_PATH/.api_key"
    chmod 644 "$DB_PATH" "$RUSTDESK_PATH/id_ed25519" "$RUSTDESK_PATH/.api_key"
    mkdir -p "$BACKUP_DIR" "$BACKUP_DIR/pre-go-20260101-000000"
    printf 'older\n' > "$BACKUP_DIR/neodesk_backup_20260101_000000.tar.gz"
    chmod 755 "$BACKUP_DIR" "$BACKUP_DIR/pre-go-20260101-000000"
    chmod 644 "$BACKUP_DIR/neodesk_backup_20260101_000000.tar.gz"
    umask 022
    do_backup_silent >/dev/null 2>&1 || fail "the backup failed"
    [ "$(umask)" = 0022 ] || fail "the backup left the umask at $(umask)"
    [ "$(stat -c %a "$BACKUP_DIR")" = 700 ] || fail "the backup folder has mode $(stat -c %a "$BACKUP_DIR")"
    local f
    for f in "$BACKUP_DIR"/neodesk_backup_*.tar.gz; do
        [ "$(stat -c %a "$f")" = 600 ] || fail "$(basename "$f") has mode $(stat -c %a "$f")"
    done
    [ "$(stat -c %a "$BACKUP_DIR/pre-go-20260101-000000")" = 700 ] || fail "the upgrade's backup folder has mode $(stat -c %a "$BACKUP_DIR/pre-go-20260101-000000")"
    f=$(ls "$BACKUP_DIR"/neodesk_backup_2*.tar.gz | tail -n 1)
    tar -tvzf "$f" > "$TEST_DIR/list" || fail "the backup can't be listed"
    grep -q 'id_ed25519$' "$TEST_DIR/list" || fail "the backup has no key: $(cat "$TEST_DIR/list")"
    if awk '{ print $1 }' "$TEST_DIR/list" | grep -v -- '------$'; then
        fail "an entry in the backup is readable by others: $(cat "$TEST_DIR/list")"
    fi
    [ ! -e "$BACKUP_DIR/$(basename "$f" .tar.gz)" ] || fail "the backup's folder stayed beside its archive"

    # A BACKUP_DIR given to the installer keeps its mode; its backups don't
    local given="$TEST_DIR/given"
    mkdir -p "$given" && chmod 755 "$given"
    printf 'older\n' > "$given/neodesk_backup_20260101_000000.tar.gz" && chmod 644 "$given/neodesk_backup_20260101_000000.tar.gz"
    BACKUP_DIR_GIVEN="$given" BACKUP_DIR="$given" secure_backup_folder
    [ "$(stat -c %a "$given")" = 755 ] || fail "a given BACKUP_DIR got mode $(stat -c %a "$given")"
    [ "$(stat -c %a "$given/neodesk_backup_20260101_000000.tar.gz")" = 600 ] || fail "a backup in a given BACKUP_DIR stayed readable"
    rm -rf "$TEST_DIR"
)

# LINUX-NATIVE-16: the newest BACKUP_KEEP (5) of the installer's backups
# stay, older ones go; BACKUP_KEEP=0 keeps every one, and other files in the
# folder stay.
test_old_backups_are_pruned() (
    load
    print_step() { :; }
    print_success() { :; }
    print_warning() { :; }
    BACKUP_DIR="$TEST_DIR/backups"
    DB_PATH="$RUSTDESK_PATH/db_v2.sqlite3"
    mkdir -p "$BACKUP_DIR"
    local day
    for day in 01 02 03 04 05 06; do
        printf 'old\n' > "$BACKUP_DIR/neodesk_backup_202601${day}_000000.tar.gz"
    done
    printf 'mine\n' > "$BACKUP_DIR/notes.txt"
    do_backup_silent >/dev/null 2>&1 || fail "the backup failed"
    [ "$(ls "$BACKUP_DIR"/neodesk_backup_*.tar.gz | wc -l)" = 5 ] || fail "not five backups stay: $(ls "$BACKUP_DIR")"
    [ ! -e "$BACKUP_DIR/neodesk_backup_20260101_000000.tar.gz" ] && [ ! -e "$BACKUP_DIR/neodesk_backup_20260102_000000.tar.gz" ] \
        || fail "the oldest backups stayed: $(ls "$BACKUP_DIR")"
    [ -e "$BACKUP_DIR/neodesk_backup_20260106_000000.tar.gz" ] || fail "a newer backup was removed: $(ls "$BACKUP_DIR")"
    [ -e "$BACKUP_DIR/notes.txt" ] || fail "a file that is no backup was removed"
    sleep 1
    BACKUP_KEEP=0 do_backup_silent >/dev/null 2>&1 || fail "the backup with BACKUP_KEEP=0 failed"
    sleep 1
    BACKUP_KEEP=0 do_backup_silent >/dev/null 2>&1 || fail "the backup with BACKUP_KEEP=0 failed"
    [ "$(ls "$BACKUP_DIR"/neodesk_backup_*.tar.gz | wc -l)" = 7 ] || fail "BACKUP_KEEP=0 removed backups: $(ls "$BACKUP_DIR")"
    rm -rf "$TEST_DIR"
)

# workflow_step NAME|ID: the run script of a step of release-go.yml, as
# bash runs it (the block below its run: |, without the YAML indent)
workflow_step() {
    awk -v want="$1" '
        /^      - (id|name): / { inside = (index($0, want) > 0); next }
        inside && /^        run: \|/ { body = 1; next }
        body && /^          / { print substr($0, 11); next }
        body && !/^[[:space:]]*$/ { exit }
        body { print "" }
    ' "$SCRIPT_ROOT/.github/workflows/release-go.yml"
}

# L210 (A183): release-go.yml publishes a v* tag with a suffix
# (v3.6.0-rc.1) as a GitHub pre-release, never the latest release; a
# version without one is the latest release.
test_release_workflow_publishes_candidates_as_prereleases() (
    local dir version plan publish
    dir=$(mktemp -d)
    plan=$(workflow_step 'id: plan' | sed 's/\${{ inputs.channel }}//')
    publish=$(workflow_step 'name: Publish')
    [ -n "$plan" ] && [ -n "$publish" ] || fail "release-go.yml has no plan or Publish step"
    for version in 3.6.0-rc.1 3.6.0; do
        mkdir -p "$dir/$version/neodesk-server/internal/productversion" "$dir/$version/dist"
        printf '%s\n' "$version" > "$dir/$version/neodesk-server/internal/productversion/VERSION"
        touch "$dir/$version/dist/neodesk-server-linux-amd64" "$dir/$version/dist/manifest.json" "$dir/$version/dist/SHA256SUMS"
        (
            cd "$dir/$version"
            SERVER_DIR=neodesk-server GITHUB_REF_TYPE=tag GITHUB_REF_NAME="v$version" GITHUB_OUTPUT="$dir/$version/out" \
                bash -c "$plan" >/dev/null
            # The outputs reach the Publish step as its env does
            export GH_LOG="$dir/$version/gh"
            gh() { printf '%s\n' "$*" >> "$GH_LOG"; [ "$1 $2" != "release view" ]; }
            export -f gh
            TAG=$(sed -n 's/^tag=//p' out) VERSION=$(sed -n 's/^version=//p' out) PRERELEASE=$(sed -n 's/^prerelease=//p' out) \
                GITHUB_SHA=0123456789abcdef0123456789abcdef01234567 bash -c "$publish"
        ) || fail "the release steps failed for $version"
    done
    grep -q -- '^release create v3.6.0-rc.1 .*--prerelease' "$dir/3.6.0-rc.1/gh" || fail "v3.6.0-rc.1 is not made a pre-release: $(cat "$dir/3.6.0-rc.1/gh")"
    if grep -q -- '--latest' "$dir/3.6.0-rc.1/gh"; then fail "v3.6.0-rc.1 is marked the latest release: $(cat "$dir/3.6.0-rc.1/gh")"; fi
    grep -q -- '^release edit v3.6.0 --draft=false --latest$' "$dir/3.6.0/gh" || fail "v3.6.0 is not the latest release: $(cat "$dir/3.6.0/gh")"
    if grep -q -- '--prerelease' "$dir/3.6.0/gh"; then fail "v3.6.0 is made a pre-release: $(cat "$dir/3.6.0/gh")"; fi
    rm -rf "$dir"
)

# A tag with release-notes/<tag>.md is published with that file, then every
# commit since the release before, folded, and the compare link; a tag
# without one gets GitHub's generated notes.
test_release_workflow_takes_the_notes_file() (
    local dir publish tag
    dir=$(mktemp -d)
    publish=$(workflow_step 'name: Publish')
    [ -n "$publish" ] || fail "release-go.yml has no Publish step"
    mkdir -p "$dir/repo/release-notes" "$dir/repo/dist" "$dir/tmp"
    cp "$SCRIPT_ROOT/release-notes/body.sh" "$dir/repo/release-notes/"
    (
        cd "$dir/repo"
        export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@localhost GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@localhost
        git init -q .
        git commit -q --allow-empty -m 'chore: release 3.6.0-rc.1' && git tag v3.6.0-rc.1
        git commit -q --allow-empty -m 'fix: keep the console signed in' && git tag v3.6.0-rc.2
        printf 'What this release is.\n' > release-notes/v3.6.0-rc.2.md
        touch dist/neodesk-server-linux-amd64 dist/manifest.json dist/SHA256SUMS
        export GH_LOG="$dir/gh"
        gh() {
            printf '%s\n' "$*" >> "$GH_LOG"
            local a
            for a in "$@"; do case "$a" in *.md) cp "$a" "$GH_LOG.notes" ;; esac; done
            [ "$1 $2" != "release view" ]
        }
        export -f gh
        for tag in v3.6.0-rc.2 v3.6.0-rc.1; do
            TAG=$tag VERSION=${tag#v} PRERELEASE=true RUNNER_TEMP="$dir/tmp" GITHUB_REPOSITORY=neodeskapp/neodesk \
                GITHUB_SHA=0123456789abcdef0123456789abcdef01234567 bash -c "$publish" || exit 1
        done
    ) || fail "the release steps failed"
    grep -q -- "^release create v3.6.0-rc.2 .*--notes-file $dir/tmp/release-notes.md\$" "$dir/gh" \
        || fail "v3.6.0-rc.2 is not published with its notes file: $(cat "$dir/gh")"
    if grep -q -- '^release create v3.6.0-rc.2 .*--generate-notes' "$dir/gh"; then fail "v3.6.0-rc.2 also gets generated notes: $(cat "$dir/gh")"; fi
    [ "$(head -n 1 "$dir/gh.notes")" = 'What this release is.' ] || fail "the notes do not start with the file: $(cat "$dir/gh.notes")"
    grep -q '^- fix: keep the console signed in (' "$dir/gh.notes" || fail "the notes lack the commits since v3.6.0-rc.1: $(cat "$dir/gh.notes")"
    if grep -q 'chore: release 3.6.0-rc.1' "$dir/gh.notes"; then fail "the notes list a commit of the release before: $(cat "$dir/gh.notes")"; fi
    grep -q '^<summary>All changes since v3.6.0-rc.1</summary>$' "$dir/gh.notes" || fail "the commits are not folded: $(cat "$dir/gh.notes")"
    grep -q 'https://github.com/neodeskapp/neodesk/compare/v3.6.0-rc.1...v3.6.0-rc.2' "$dir/gh.notes" || fail "the notes lack the compare link: $(cat "$dir/gh.notes")"
    grep -q -- '^release create v3.6.0-rc.1 .*--generate-notes$' "$dir/gh" || fail "a tag without a notes file does not get generated notes: $(cat "$dir/gh")"
    rm -rf "$dir"
)

# L210 (A183): while the repository has no stable release, the stable
# channel installs the newest pre-release that is not the rolling dev one
# (a release candidate), also with --channel stable or the channel saved in
# .env, and stays on the stable channel; the run says which release it took.
test_no_stable_release_installs_the_newest_prerelease() (
    unset UPDATE_GITHUB_OWNER UPDATE_GITHUB_REPO RELEASE_BASE_URL
    load
    print_info() { echo "$1"; }
    print_warning() { echo "$1"; }
    print_error() { echo "$1"; }
    release_asset_name() { echo neodesk-server-linux-amd64; }
    SKIP_VERIFY=true
    # curl: no stable release; the list of releases, newest first: the dev
    # pre-release, a draft, two release candidates
    curl() {
        local a prev="" dest="" url="${!#}"
        for a in "$@"; do [ "$prev" = -o ] && dest="$a"; prev="$a"; done
        case "$url" in
            https://api.github.com/repos/neodeskapp/neodesk/releases?per_page=50)
                printf '[\n  {\n    "url": "x",\n    "author": {"login": "bot"},\n    "tag_name": "dev",\n    "draft": false,\n    "prerelease": true,\n    "assets": [{"name": "manifest.json"}]\n  },\n'
                printf '  {"tag_name": "v3.7.0-rc.1", "draft": true, "prerelease": true, "assets": []},\n'
                printf '  {"tag_name": "v3.6.0-rc.2", "name": "NeoDesk 3.6.0-rc.2", "draft": false, "prerelease": true, "assets": []},\n'
                printf '  {"tag_name": "v3.6.0-rc.1", "draft": false, "prerelease": true, "assets": []}\n]\n' ;;
            */releases/download/v3.6.0-rc.2/SHA256SUMS)
                printf '%s  neodesk-server-linux-amd64\n' "$(printf 'candidate\n' | sha256sum | cut -d' ' -f1)" > "$dest"
                printf '200' ;;
            */releases/download/v3.6.0-rc.2/neodesk-server-linux-amd64) printf 'candidate\n' > "$dest"; printf '200' ;;
            *) printf '404' ;;
        esac
    }
    local given
    for given in "" main saved; do
        rm -f "$TEST_DIR/neodesk-server" "$CONSOLE_PATH/.env"
        unset UPDATE_GITHUB_BRANCH_GIVEN
        UPDATE_GITHUB_BRANCH=main
        case "$given" in
            main) UPDATE_GITHUB_BRANCH_GIVEN=main ;;
            saved) printf 'UPDATE_GITHUB_BRANCH=main\n' > "$CONSOLE_PATH/.env" ;;
        esac
        download_release_binary "$TEST_DIR" > "$TEST_DIR/out" 2>&1 || fail "no stable release and the candidate was not installed ($given): $(cat "$TEST_DIR/out")"
        [ "$(cat "$TEST_DIR/neodesk-server")" = candidate ] || fail "the newest pre-release is not staged ($given)"
        grep -q 'no stable release yet: installing the newest pre-release v3.6.0-rc.2' "$TEST_DIR/out" \
            || fail "the run does not say which release it took ($given): $(cat "$TEST_DIR/out")"
        [ "$UPDATE_GITHUB_BRANCH" = main ] || fail "the install left the stable channel for $UPDATE_GITHUB_BRANCH ($given)"
    done
    rm -rf "$TEST_DIR"
)

# A fresh install from the one-liner without --channel takes the stable
# channel, and while the repository has no stable release and no
# pre-release it installs the development build, says so and keeps that
# channel. A channel given (--channel stable) or saved in .env gets no
# fallback but the error.
test_no_stable_release_installs_dev() (
    unset UPDATE_GITHUB_OWNER UPDATE_GITHUB_REPO RELEASE_BASE_URL
    load
    print_info() { echo "$1"; }
    print_warning() { echo "$1"; }
    print_error() { echo "$1"; }
    release_asset_name() { echo neodesk-server-linux-amd64; }
    unset UPDATE_GITHUB_BRANCH_GIVEN
    UPDATE_GITHUB_BRANCH=main
    SKIP_VERIFY=true
    # curl: no stable release, a development build
    curl() {
        local a prev="" dest="" url="${!#}"
        for a in "$@"; do [ "$prev" = -o ] && dest="$a"; prev="$a"; done
        case "$url" in
            */releases/latest/download/*) printf '404' ;;
            */releases/download/dev/SHA256SUMS)
                printf '%s  neodesk-server-linux-amd64\n' "$(printf 'dev build\n' | sha256sum | cut -d' ' -f1)" > "$dest"
                printf '200' ;;
            */releases/download/dev/neodesk-server-linux-amd64) printf 'dev build\n' > "$dest"; printf '200' ;;
            *) printf '404' ;;
        esac
    }
    download_release_binary "$TEST_DIR" > "$TEST_DIR/out" 2>&1 || fail "no stable release and no fallback: $(cat "$TEST_DIR/out")"
    [ -f "$TEST_DIR/neodesk-server" ] || fail "the development build is not staged"
    grep -q 'no stable release or pre-release yet: installing the development build' "$TEST_DIR/out" || fail "the fallback is not said: $(cat "$TEST_DIR/out")"
    grep -q -- '--update --channel stable' "$TEST_DIR/out" || fail "the fallback does not say how to go back to stable: $(cat "$TEST_DIR/out")"
    [ "$UPDATE_GITHUB_BRANCH" = dev ] && [ "$UPDATE_GITHUB_BRANCH_GIVEN" = dev ] \
        || fail "the fallback does not keep the development channel ($UPDATE_GITHUB_BRANCH, given ${UPDATE_GITHUB_BRANCH_GIVEN:-none})"
    printf 'SESSION_SECRET=kept\n' > "$CONSOLE_PATH/.env"
    save_given_update_channel
    grep -qx 'UPDATE_GITHUB_BRANCH=dev' "$CONSOLE_PATH/.env" || fail "the development channel does not reach .env"

    local out
    rm -f "$TEST_DIR/neodesk-server"
    UPDATE_GITHUB_BRANCH=main
    UPDATE_GITHUB_BRANCH_GIVEN=main
    out=$(download_release_binary "$TEST_DIR" 2>&1) && fail "--channel stable installed the development build: $out"
    case "$out" in *"no stable release yet"*"--channel dev"*) ;; *) fail "--channel stable without a release says: $out" ;; esac
    case "$out" in *"installing the development build"*) fail "--channel stable fell back: $out" ;; esac
    unset UPDATE_GITHUB_BRANCH_GIVEN
    printf 'UPDATE_GITHUB_BRANCH=main\n' > "$CONSOLE_PATH/.env"
    out=$(download_release_binary "$TEST_DIR" 2>&1) && fail "the channel saved in .env fell back: $out"
    rm -rf "$TEST_DIR"
)

# L143: a second run updates instead of installing again, the password
# shows only when the install made it, and after the first start .env and
# its copies keep no admin password.
test_rerun_updates_and_forgets_the_password() (
    load
    [ "$(auto_action)" = install ] || fail "a machine without NeoDesk gets $(auto_action)"
    write_server_unit "-mode all" ""
    printf '#!/bin/sh\n' > "$RUSTDESK_PATH/neodesk-server"
    chmod +x "$RUSTDESK_PATH/neodesk-server"
    printf 'DEFAULT_ADMIN_PASSWORD=First-Pass-1\nINIT_ADMIN_PASS=First-Pass-1\nSESSION_SECRET=kept\n' > "$CONSOLE_PATH/.env"
    cp "$CONSOLE_PATH/.env" "$CONSOLE_PATH/.env.pre-go"
    [ "$(auto_action)" = update ] || fail "a second run on an install gets $(auto_action)"
    USE_POSTGRESQL=true
    [ "$(auto_action)" = install ] || fail "--postgresql on a SQLite install gets $(auto_action), not the move"
    USE_POSTGRESQL=false
    UNINSTALL_MODE=true
    [ "$(auto_action)" = uninstall ] || fail "--uninstall on an install gets $(auto_action)"
    UNINSTALL_MODE=false

    local out
    FRESH_CONSOLE_ENV=false
    ADMIN_PASSWORD=""
    out=$(create_admin_user 2>&1)
    case "$out" in *First-Pass-1*) fail "a run that kept .env showed the admin password again" ;; esac
    FRESH_CONSOLE_ENV=true
    out=$(create_admin_user 2>&1)
    case "$out" in *First-Pass-1*) ;; *) fail "a fresh install did not show the admin password" ;; esac

    forget_first_admin_password
    for f in "$CONSOLE_PATH/.env" "$CONSOLE_PATH/.env.pre-go"; do
        if grep -q 'First-Pass-1' "$f"; then fail "$(basename "$f") keeps the admin password after the first start"; fi
    done
    grep -qx 'DEFAULT_ADMIN_PASSWORD=' "$CONSOLE_PATH/.env" || fail "DEFAULT_ADMIN_PASSWORD is not left empty"
    grep -qx 'SESSION_SECRET=kept' "$CONSOLE_PATH/.env" || fail "forgetting the password lost a line of .env"
    rm -rf "$TEST_DIR"
)

# L69: the host clients get becomes PANEL_PUBLIC_HOST, so the HTTP port
# redirects to HTTPS there; a value the admin set stays.
test_panel_public_host() (
    load
    systemctl() { :; }
    printf 'SESSION_SECRET=kept\n' > "$CONSOLE_PATH/.env"
    set_panel_public_host "203.0.113.7:21117"
    grep -qx 'PANEL_PUBLIC_HOST=203.0.113.7' "$CONSOLE_PATH/.env" || fail "PANEL_PUBLIC_HOST from host:port: $(grep PANEL_PUBLIC_HOST "$CONSOLE_PATH/.env")"
    set_panel_public_host "198.51.100.1"
    grep -qx 'PANEL_PUBLIC_HOST=203.0.113.7' "$CONSOLE_PATH/.env" || fail "a second address replaced PANEL_PUBLIC_HOST"
    printf 'PANEL_PUBLIC_HOST=\n' > "$CONSOLE_PATH/.env"
    write_server_unit "-mode all -relay-servers desk.example.org -api-port 21114" ""
    maybe_update_services >/dev/null
    grep -qx 'PANEL_PUBLIC_HOST=desk.example.org' "$CONSOLE_PATH/.env" || fail "an update set no PANEL_PUBLIC_HOST: $(cat "$CONSOLE_PATH/.env")"
    rm -rf "$TEST_DIR"
)

# L149: the end of an install names the console's address, the first
# sign-in, the client settings with the whole key, the log, the settings
# file and the commands for update and uninstall, and shows no password.
test_install_summary() (
    load
    printf '[Service]
Environment=NEODESK_SUPERVISOR=systemd
' > "$SERVER_UNIT"
    printf 'PANEL_PUBLIC_HOST=desk.example.org\nHTTPS_ENABLED=true\nHTTPS_PORT=5443\nCONSOLE_PORT=5000\nDEFAULT_ADMIN_PASSWORD=\n' > "$CONSOLE_PATH/.env"
    printf 'AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJ=\n' > "$RUSTDESK_PATH/id_ed25519.pub"
    ADMIN_PASSWORD='Summary-Pass-1'
    local out
    out=$(print_install_summary 2>&1)
    # L76: clients get the client API port, plain HTTP while the
    # certificate is self-signed (RustDesk refuses it)
    printf 'API_PORT=21121\nRUSTDESK_API_TLS=false\n' >> "$CONSOLE_PATH/.env"
    out=$(print_install_summary 2>&1)
    case "$out" in *"API server:    "*"http://desk.example.org:21121"*) ;; *) fail "the summary gives clients another API address: $out" ;; esac
    for want in "https://desk.example.org:5443" "admin" "ID server" "desk.example.org" \
        "AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJ=" "journalctl -u neodesk-server" "$CONSOLE_PATH/.env" \
        "neodesk-setup --update" "neodesk-setup --uninstall"; do
        case "$out" in *"$want"*) ;; *) fail "the summary does not name $want: $out" ;; esac
    done
    case "$out" in *Summary-Pass-1*) fail "the summary shows the password" ;; esac
    # LINUX-NATIVE-19: the relay with its port, as the console shows it
    case "$out" in *"Relay server:  desk.example.org:21117"*) ;; *) fail "the summary gives the relay without its port: $out" ;; esac
    # LINUX-NATIVE-8: the first sign-in says how to get a new password
    FRESH_CONSOLE_ENV=true
    out=$(print_install_summary 2>&1)
    case "$out" in *"First sign-in"*"neodesk-setup --reset-password"*) ;; *) fail "the first sign-in does not say how to get a new password: $out" ;; esac
    rm -rf "$TEST_DIR"
)

# LINUX-NATIVE-8: the password box names the command that gives a new
# password, its right border stays in line, and the run does not talk about
# STORE_ADMIN_CREDENTIALS.
test_password_box() (
    load
    local LC_ALL=C.UTF-8
    FRESH_CONSOLE_ENV=true
    ADMIN_PASSWORD=0123456789abcdef0123456789abcdef
    local out widths
    out=$(create_admin_user 2>&1)
    case "$out" in *"$ADMIN_PASSWORD"*) ;; *) fail "the box does not show the password: $out" ;; esac
    case "$out" in *"Lost it? sudo neodesk-setup --reset-password"*) ;; *) fail "the box does not say how to get a new password: $out" ;; esac
    case "$out" in *STORE_ADMIN_CREDENTIALS*|*"not persisted"*) fail "the run talks about storing the credentials: $out" ;; esac
    widths=$(printf '%s\n' "$out" | grep '[║╔╚╠]' | while IFS= read -r line; do printf '%s\n' "${#line}"; done | sort -u)
    [ "$(printf '%s\n' "$widths" | wc -l)" = 1 ] || fail "the box's lines differ in width: $widths
$out"
    rm -rf "$TEST_DIR"
)

# LINUX-NATIVE-3, -12: an interactive install asks every question before
# it installs anything, the self-signed certificate is made after the relay
# address is known, and the password and the summary come last.
test_interactive_install_asks_first() (
    load
    AUTO_MODE=false
    MINIMAL_MODE=false
    RELAY_SERVERS=""
    RELAY_MODE=auto
    FRESH_CONSOLE_ENV=true
    ADMIN_PASSWORD=Interactive-Pass-1
    print_header() { :; }
    print_step() { :; }
    print_success() { echo "$1"; }
    detect_installation() { INSTALL_STATUS=none; }
    is_node_era_install() { false; }
    menu_choose() { MENU_CHOICE=1; }
    get_local_ip() { echo 192.168.1.5; }
    graceful_stop_services() { :; }
    install_dependencies() { echo "INSTALLING"; }
    detect_architecture() { :; }
    install_binaries() { :; }
    setup_console_home() { :; }
    generate_ssl_certificates() { echo "CERTIFICATE for ${RELAY_SERVERS:-none}"; }
    setup_services() { :; }
    configure_firewall_rules() { :; }
    start_services() { :; }
    forget_first_admin_password() { :; }
    do_configure_reverse_proxy() { echo "PROXY STEP"; }
    install_manager_command() { :; }
    print_install_summary() { echo "SUMMARY"; }
    local out
    # Relay: a custom address; P2P first; a reverse proxy: yes
    out=$(printf '3\nrd.example.net\n1\ny\n' | do_install 2>&1) || fail "the install failed: $out"
    printf '%s\n' "$out" > "$TEST_DIR/out"
    local relay_q conn_q tls_q install cert proxy box summary
    relay_q=$(grep -n 'Select relay mode' "$TEST_DIR/out" | head -1 | cut -d: -f1)
    conn_q=$(grep -n 'Select connection mode' "$TEST_DIR/out" | head -1 | cut -d: -f1)
    tls_q=$(grep -n 'reverse proxy' "$TEST_DIR/out" | head -1 | cut -d: -f1)
    install=$(grep -n '^INSTALLING$' "$TEST_DIR/out" | cut -d: -f1)
    cert=$(grep -n '^CERTIFICATE for rd.example.net$' "$TEST_DIR/out" | cut -d: -f1)
    proxy=$(grep -n '^PROXY STEP$' "$TEST_DIR/out" | cut -d: -f1)
    box=$(grep -n 'Interactive-Pass-1' "$TEST_DIR/out" | cut -d: -f1)
    summary=$(grep -n '^SUMMARY$' "$TEST_DIR/out" | cut -d: -f1)
    [ -n "$relay_q" ] && [ -n "$conn_q" ] && [ -n "$tls_q" ] && [ -n "$install" ] && [ -n "$cert" ] \
        && [ -n "$proxy" ] && [ -n "$box" ] && [ -n "$summary" ] || fail "a step is missing: $out"
    [ "$relay_q" -lt "$install" ] && [ "$conn_q" -lt "$install" ] && [ "$tls_q" -lt "$install" ] \
        || fail "a question came after the install started: $out"
    [ "$proxy" -lt "$box" ] && [ "$box" -lt "$summary" ] || fail "the proxy step came after the password or the summary: $out"
    [ "$(grep -v '^[[:space:]]*$' "$TEST_DIR/out" | tail -n 1)" = SUMMARY ] || fail "the summary is not the last thing: $out"
    rm -rf "$TEST_DIR"
)

# LINUX-NATIVE-3: a relay address given by name goes into the self-signed
# certificate and becomes its common name.
test_certificate_names_the_relay_address() (
    load
    command -v openssl >/dev/null || return 0
    print_success() { :; }
    get_public_ip() { echo 203.0.113.7; }
    deploy_ssl_material_to_rustdesk_dir() { :; }
    RELAY_SERVERS=rd.example.net:21117
    unset SSL_DOMAIN
    generate_ssl_certificates >/dev/null 2>&1 || fail "the certificate was not made"
    local text subject
    text=$(openssl x509 -in "$RUSTDESK_PATH/ssl/neodesk.crt" -noout -text)
    case "$text" in *"DNS:rd.example.net"*) ;; *) fail "the certificate is not for the relay address: $text" ;; esac
    case "$text" in *"IP Address:203.0.113.7"*) ;; *) fail "the certificate lost the public IP: $text" ;; esac
    subject=$(openssl x509 -in "$RUSTDESK_PATH/ssl/neodesk.crt" -noout -subject)
    case "$subject" in *"CN = rd.example.net"*|*"CN=rd.example.net"*) ;; *) fail "the common name: $subject" ;; esac
    [ "$(san_entry 2001:db8::1)" = "IP:2001:db8::1" ] && [ "$(san_entry 198.51.100.4)" = "IP:198.51.100.4" ] \
        || fail "san_entry of an address"
    [ "$(address_host '[2001:db8::1]:21117')" = 2001:db8::1 ] && [ "$(address_host 'a.example:1,b.example')" = a.example ] \
        || fail "address_host"
    rm -rf "$TEST_DIR"
)

# LINUX-NATIVE-13: a fixed relay address is reported as mode fixed.
test_fixed_relay_mode_is_said() (
    load
    systemctl() { :; }
    print_success() { :; }
    print_info() { echo "$1"; }
    remove_retired_units() { :; }
    AUTO_MODE=true
    RELAY_SERVERS=rd.example.net
    printf 'SESSION_SECRET=kept\n' > "$CONSOLE_PATH/.env"
    local out
    out=$(setup_services 2>&1)
    case "$out" in *"Relay server IP: rd.example.net (mode: fixed)"*) ;; *) fail "the relay mode line: $out" ;; esac
    rm -rf "$TEST_DIR"
)

# LINUX-NATIVE-6: the PostgreSQL question offers this machine or an
# existing database by URI; a URI skips setting PostgreSQL up here, and a
# database host elsewhere without a URI stops instead of making the
# database on this machine.
test_postgresql_question() (
    load
    print_warning() { echo "$1"; }
    install_postgresql() { echo install_postgresql >> "$TEST_DIR/calls"; }
    setup_postgresql_database() { echo setup_postgresql_database >> "$TEST_DIR/calls"; }
    AUTO_MODE=false
    USE_POSTGRESQL=false
    POSTGRESQL_URI=""
    # The first menu: PostgreSQL; the second: an existing database
    menu_choose() { MENU_CHOICE=2; }
    printf 'db.example.org:5432\npostgresql://nd:Pass@db.example.org:5432/desk\n' > "$TEST_DIR/in"
    choose_database_type > "$TEST_DIR/out" 2>&1 < "$TEST_DIR/in"
    [ "$USE_POSTGRESQL" = true ] && [ "$POSTGRESQL_URI" = postgresql://nd:Pass@db.example.org:5432/desk ] \
        || fail "the URI was not taken: ${POSTGRESQL_URI:-none} $(cat "$TEST_DIR/out")"
    grep -q 'postgres:// or postgresql://' "$TEST_DIR/out" || fail "a line that is no URI was not refused: $(cat "$TEST_DIR/out")"
    if grep -qi 'host \[' "$TEST_DIR/out"; then fail "the question still asks for a host: $(cat "$TEST_DIR/out")"; fi
    prepare_postgresql >/dev/null 2>&1 || fail "the database given by URI failed"
    [ ! -s "$TEST_DIR/calls" ] || fail "a database given by URI was set up here: $(cat "$TEST_DIR/calls")"

    local out
    POSTGRESQL_URI=""
    POSTGRESQL_HOST=db.example.org
    out=$(prepare_postgresql 2>&1) && fail "a database host elsewhere without a URI was set up here"
    case "$out" in *--pg-uri*) ;; *) fail "the refusal does not name --pg-uri: $out" ;; esac
    [ ! -s "$TEST_DIR/calls" ] || fail "PostgreSQL was installed for a host elsewhere: $(cat "$TEST_DIR/calls")"
    rm -rf "$TEST_DIR"
)

# L149: the uninstall says what it leaves on the machine, and --purge
# removes the data and the commands too: neodesk-setup, and neodesk when
# it is the copy of the server binary or an earlier build's link to it
# (L431), but not a neodesk of someone else.
test_uninstall_says_what_stays() (
    load
    systemctl() { :; }
    do_backup_silent() { :; }
    print_header() { :; }
    AUTO_MODE=true
    UNINSTALL_MODE=true
    commands_in_test_dir
    mkdir -p "$TEST_DIR/sbin" "$TEST_DIR/bin" && printf '#!/bin/sh\n' > "$MANAGER_COMMAND" && chmod +x "$MANAGER_COMMAND"
    fake_handing_binary
    cp "$RUSTDESK_PATH/neodesk-server" "$CLI_COMMAND"
    printf 'DB_TYPE=sqlite\n' > "$CONSOLE_PATH/.env"
    local out
    out=$(do_uninstall 2>&1)
    for want in "Left on this machine" "$RUSTDESK_PATH" "$CONSOLE_PATH" "--purge" "$MANAGER_COMMAND" "$CLI_COMMAND" \
        "neodesk-setup --uninstall --purge"; do
        case "$out" in *"$want"*) ;; *) fail "the uninstall does not name $want: $out" ;; esac
    done
    [ -d "$RUSTDESK_PATH" ] && [ -d "$CONSOLE_PATH" ] || fail "the uninstall without --purge removed the data"
    PURGE_MODE=true
    do_uninstall >/dev/null 2>&1
    [ ! -e "$RUSTDESK_PATH" ] && [ ! -e "$CONSOLE_PATH" ] && [ ! -e "$MANAGER_COMMAND" ] && [ ! -e "$CLI_COMMAND" ] \
        || fail "--purge left $(ls -d "$RUSTDESK_PATH" "$CONSOLE_PATH" "$MANAGER_COMMAND" "$CLI_COMMAND" 2>/dev/null | tr '\n' ' ')"
    mkdir -p "$RUSTDESK_PATH" "$CONSOLE_PATH"
    printf '#!/bin/sh\necho mine\n' > "$CLI_COMMAND"
    do_uninstall >/dev/null 2>&1
    grep -qx 'echo mine' "$CLI_COMMAND" || fail "--purge removed a neodesk of someone else"
    rm -rf "$TEST_DIR"
)

# LINUX-NATIVE-1, LINUX-NATIVE-7: --uninstall --purge on a terminal offers
# to remove the backups and then makes none; without a terminal it keeps
# them and says so. The list names the PostgreSQL database the settings
# named, with the commands that remove it.
test_purge_handles_backups_and_the_database() (
    load
    systemctl() { :; }
    print_header() { :; }
    AUTO_MODE=true
    UNINSTALL_MODE=true
    PURGE_MODE=true
    commands_in_test_dir
    BACKUP_DIR="$TEST_DIR/backups"
    mkdir -p "$BACKUP_DIR"
    printf 'old\n' > "$BACKUP_DIR/neodesk_backup_20260101_000000.tar.gz"
    printf 'DATABASE_URL=postgres://neodesk:secret@localhost:5432/neodesk?sslmode=disable\n' > "$CONSOLE_PATH/.env"
    do_backup_silent() { echo "a backup was made" >> "$TEST_DIR/calls"; }
    has_terminal() { true; }
    local out
    out=$(printf 'y\n' | do_uninstall 2>&1)
    [ ! -e "$BACKUP_DIR" ] || fail "purge with yes to the backups left $(ls -A "$BACKUP_DIR")"
    if grep -q 'a backup was made' "$TEST_DIR/calls" 2>/dev/null; then fail "a purge that removes the backups made one"; fi
    case "$out" in *"sudo -u postgres dropdb neodesk && sudo -u postgres dropuser neodesk"*) ;; *) fail "the purge does not name the PostgreSQL database: $out" ;; esac
    case "$out" in *secret*) fail "the uninstall shows the database password: $out" ;; esac

    # No terminal: the backups stay, a new one is made, the list says where
    mkdir -p "$RUSTDESK_PATH" "$CONSOLE_PATH" "$BACKUP_DIR"
    printf 'old\n' > "$BACKUP_DIR/neodesk_backup_20260101_000000.tar.gz"
    printf 'DATABASE_URL=postgres://nd:secret@db.example.org/desk\n' > "$CONSOLE_PATH/.env"
    has_terminal() { false; }
    out=$(do_uninstall 2>&1 < /dev/null)
    [ -e "$BACKUP_DIR/neodesk_backup_20260101_000000.tar.gz" ] || fail "a purge without a terminal removed the backups"
    grep -q 'a backup was made' "$TEST_DIR/calls" || fail "a purge without a terminal made no backup"
    case "$out" in *"$BACKUP_DIR: the backups, readable by root only"*) ;; *) fail "the list does not name the backups: $out" ;; esac
    case "$out" in *"the PostgreSQL database desk on db.example.org"*) ;; *) fail "the list does not name the remote database: $out" ;; esac
    rm -rf "$TEST_DIR"
)

# LINUX-NATIVE-16, LINUX-NATIVE-17: an update that finds the release
# installed already makes no backup, and when the unit and .env stay as
# they were the running server is not restarted. A new release gets the
# backup first, the restart, and --no-restart-hint when the binary has it.
# The unit's description names no version.
test_update_without_a_new_release_changes_nothing() (
    load
    print_step() { :; }
    print_success() { :; }
    print_warning() { :; }
    print_error() { echo "$1"; }
    SERVER_BINARY_FILE=""
    FROM_SOURCE=false
    unset RELEASE_BASE_URL
    printf '#!/bin/sh\n# upgrade-env --from FILE; verify-release --manifest FILE; update [--no-restart-hint] (its commands)\necho neodesk-server 3.6.0\n' > "$RUSTDESK_PATH/neodesk-server"
    chmod +x "$RUSTDESK_PATH/neodesk-server"
    write_server_unit "-mode all -api-port 21114" ""
    grep -qx 'Description=NeoDesk server and web console' "$SERVER_UNIT" || fail "the unit's description: $(grep '^Description=' "$SERVER_UNIT")"
    printf 'SESSION_SECRET=kept\n' > "$CONSOLE_PATH/.env"
    do_backup_silent() { echo "backup" >> "$TEST_DIR/calls"; }
    merge_console_env() { :; }
    set_server_folder_permissions() { :; }
    install_manager_command() { :; }
    verify_go_install() { :; }
    systemctl() {
        case "$1" in
            is-active) return 0 ;;
            *) echo "systemctl $*" >> "$TEST_DIR/calls" ;;
        esac
    }
    server_cmd() {
        echo "server $*" >> "$TEST_DIR/calls"
        case "$*" in *"--check"*) echo "Already up to date (2222222)." ;; esac
    }
    update_go_install >/dev/null 2>&1 || fail "an update without a new release failed"
    if grep -q '^backup' "$TEST_DIR/calls"; then fail "an update without a new release made a backup: $(cat "$TEST_DIR/calls")"; fi
    if grep -q 'systemctl restart' "$TEST_DIR/calls"; then fail "an update that changed nothing restarted the server: $(cat "$TEST_DIR/calls")"; fi
    if grep -q -- '--yes' "$TEST_DIR/calls"; then fail "an update without a new release ran the install: $(cat "$TEST_DIR/calls")"; fi

    # A change to .env restarts it, still without a backup
    : > "$TEST_DIR/calls"
    merge_console_env() { printf 'NEW_KEY=1\n' >> "$CONSOLE_PATH/.env"; }
    update_go_install >/dev/null 2>&1 || fail "an update that changed .env failed"
    grep -q 'systemctl restart neodesk-server' "$TEST_DIR/calls" || fail "a changed .env did not restart the server: $(cat "$TEST_DIR/calls")"
    if grep -q '^backup' "$TEST_DIR/calls"; then fail "an update with the same binary made a backup"; fi

    # A new release: the backup first, then the install with --no-restart-hint
    : > "$TEST_DIR/calls"
    merge_console_env() { :; }
    server_cmd() {
        echo "server $*" >> "$TEST_DIR/calls"
        case "$*" in
            *"--check"*) echo "Release: neodesk-server 3.6.1" ;;
            *"--yes"*) printf '#!/bin/sh\n# upgrade-env --from FILE; verify-release --manifest FILE\necho neodesk-server 3.6.1\n' > "$RUSTDESK_PATH/neodesk-server" ;;
        esac
    }
    update_go_install >/dev/null 2>&1 || fail "the update to a new release failed"
    [ "$(grep -n '' "$TEST_DIR/calls" | grep -m1 -E 'backup|--yes' | cut -d: -f2-)" = backup ] || fail "the backup did not come before the install: $(cat "$TEST_DIR/calls")"
    grep -q -- 'update --yes --no-restart-hint' "$TEST_DIR/calls" || fail "the install did not get --no-restart-hint: $(cat "$TEST_DIR/calls")"
    grep -q 'systemctl restart neodesk-server' "$TEST_DIR/calls" || fail "a new release did not restart the server"

    # A failed check stops before the backup
    : > "$TEST_DIR/calls"
    server_cmd() { echo "server $*" >> "$TEST_DIR/calls"; echo "Update failed: GitHub API 404"; return 1; }
    local out
    out=$(update_go_install 2>&1) && fail "an update whose check failed reported success"
    case "$out" in *"GitHub API 404"*) ;; *) fail "the failed check is not shown: $out" ;; esac
    if grep -q '^backup' "$TEST_DIR/calls"; then fail "an update whose check failed made a backup"; fi
    rm -rf "$TEST_DIR"
)

# L155: the server's updater is told the repository of the releases, so an
# installed binary with an older default does not ask upstream; one named
# in .env stays.
test_update_asks_the_fork() (
    unset UPDATE_GITHUB_OWNER UPDATE_GITHUB_REPO
    load
    do_backup_silent() { :; }
    print_step() { :; }
    print_error() { :; }
    SERVER_BINARY_FILE=""
    FROM_SOURCE=false
    unset RELEASE_BASE_URL
    fake_current_binary
    server_cmd() { printf '%s/%s\n' "${UPDATE_GITHUB_OWNER:-}" "${UPDATE_GITHUB_REPO:-}" > "$TEST_DIR/seen"; return 1; }
    printf 'SESSION_SECRET=kept\n' > "$CONSOLE_PATH/.env"
    update_go_install >/dev/null 2>&1 || true
    [ "$(cat "$TEST_DIR/seen" 2>/dev/null)" = neodeskapp/neodesk ] \
        || fail "neodesk-server update ran for $(cat "$TEST_DIR/seen" 2>/dev/null || echo 'no repository')"
    printf 'UPDATE_GITHUB_OWNER=acme\nUPDATE_GITHUB_REPO=desk\n' > "$CONSOLE_PATH/.env"
    update_go_install >/dev/null 2>&1 || true
    [ "$(cat "$TEST_DIR/seen" 2>/dev/null)" = acme/desk ] || fail "the repository of .env lost to the default: $(cat "$TEST_DIR/seen")"
    rm -rf "$TEST_DIR"
)

# L67: both installers open the same ports: the API port of stock RustDesk
# clients without an API server setting (21114), RustDesk's (21115 to 21117
# TCP, 21116 UDP), the client API port, 21122 while CDAP is on, the relay
# nodes' control port (21123, off with NODE_CONTROL_PORT=0), and the
# console's ports unless TLS ends at a reverse proxy.
test_firewall_ports() (
    load
    # An install of the server with its console: CONSOLE_PORT is the
    # console's port, and one set wins over the default (3375)
    printf '[Service]\nEnvironment=NEODESK_SUPERVISOR=systemd\n' > "$SERVER_UNIT"
    printf 'API_PORT=21121\nCONSOLE_PORT=5000\nHTTPS_ENABLED=true\nHTTPS_PORT=5443\nCDAP_ENABLED=Y\nTRUST_PROXY=false\n' > "$CONSOLE_PATH/.env"
    [ "$(firewall_ports | tr '\n' ' ')" = "21114/tcp 21115/tcp 21116/tcp 21116/udp 21117/tcp 21121/tcp 21122/tcp 21123/tcp 5000/tcp 5443/tcp " ] \
        || fail "the firewall ports: $(firewall_ports | tr '\n' ' ')"
    printf 'API_PORT=21121\nCONSOLE_PORT=5000\nHTTPS_ENABLED=false\nCDAP_ENABLED=N\nTRUST_PROXY=Y\nNODE_CONTROL_PORT=0\n' > "$CONSOLE_PATH/.env"
    [ "$(firewall_ports | tr '\n' ' ')" = "21114/tcp 21115/tcp 21116/tcp 21116/udp 21117/tcp 21121/tcp " ] \
        || fail "the firewall ports behind a reverse proxy without CDAP: $(firewall_ports | tr '\n' ' ')"
    rm -rf "$TEST_DIR"
)

# L225: the install opens the console's ports before it asks about a
# reverse proxy; when TLS then ends at a proxy on this machine, the
# console's ports are closed again. A proxy on another machine still needs
# them.
test_reverse_proxy_closes_the_console_ports() (
    load
    printf '[Service]
Environment=NEODESK_SUPERVISOR=systemd
' > "$SERVER_UNIT"
    printf 'CONSOLE_PORT=5000\nHTTPS_ENABLED=true\nHTTPS_PORT=5443\n' > "$CONSOLE_PATH/.env"
    ufw() { printf '%s\n' "$*" >> "$TEST_DIR/ufw"; [ "$1" != status ] || echo "Status: active"; }
    generate_reverse_proxy_config() { REVERSE_PROXY_PANEL_BIND="$BIND"; }
    apply_console_reverse_proxy_mode() { :; }
    print_success() { :; }
    print_step() { :; }
    BIND=127.0.0.1 do_configure_reverse_proxy <<< "console.example.com" > /dev/null 2>&1 || fail "the reverse proxy set-up failed"
    grep -qx 'delete allow 5000/tcp' "$TEST_DIR/ufw" && grep -qx 'delete allow 5443/tcp' "$TEST_DIR/ufw" \
        || fail "a proxy on this machine left the console's ports open: $(cat "$TEST_DIR/ufw" 2>/dev/null)"
    rm -f "$TEST_DIR/ufw"
    BIND=0.0.0.0 do_configure_reverse_proxy <<< "console.example.com" > /dev/null 2>&1 || fail "the reverse proxy set-up failed"
    if grep -q '^delete' "$TEST_DIR/ufw" 2>/dev/null; then fail "a proxy on another machine closed the console's ports: $(cat "$TEST_DIR/ufw")"; fi
    rm -rf "$TEST_DIR"
)

# L307: an installed ufw that is off is no active firewall: neodesk.sh
# writes no rules into it (they would go live with the next "ufw enable")
# and its diagnostics do not report it as active
test_inactive_ufw_is_left_alone() (
    load
    printf '[Service]
Environment=NEODESK_SUPERVISOR=systemd
' > "$SERVER_UNIT"
    printf 'CONSOLE_PORT=5000\nHTTPS_ENABLED=false\n' > "$CONSOLE_PATH/.env"
    ufw() { printf '%s\n' "$*" >> "$TEST_DIR/ufw"; [ "$1" != status ] || echo "Status: inactive"; }
    systemctl() { return 3; }
    # iptables has no rules: -C and -D find none
    iptables() { case "$1" in -C|-D) return 1 ;; esac; }
    print_success() { :; }
    configure_firewall_rules > /dev/null 2>&1 || fail "the firewall step failed"
    close_console_firewall_ports > /dev/null 2>&1 || fail "closing the console's ports failed"
    if grep -v '^status' "$TEST_DIR/ufw" | grep -q .; then
        fail "neodesk.sh changed an inactive ufw: $(tr '\n' ';' < "$TEST_DIR/ufw")"
    fi
    print_header() { :; }
    print_status() { :; }
    journalctl() { :; }
    ss() { :; }
    netstat() { :; }
    curl() { return 7; }
    menu_choose() { MENU_CHOICE=0; }
    DB_PATH="$TEST_DIR/missing.sqlite3"
    do_diagnostics > "$TEST_DIR/out" 2>&1 || fail "the diagnostics failed: $(cat "$TEST_DIR/out")"
    if grep -q 'UFW (active)' "$TEST_DIR/out"; then
        fail "the diagnostics call an inactive ufw active: $(grep -A3 'Firewall:' "$TEST_DIR/out")"
    fi
    grep -q 'No active firewall detected' "$TEST_DIR/out" || fail "the diagnostics name no firewall state: $(cat "$TEST_DIR/out")"
    rm -rf "$TEST_DIR"
)

# L343: a ufw or firewalld that is installed but switched off is left
# alone, also by the iptables fallback: no ACCEPT rules, nothing saved to
# rules.v4. iptables gets the rules only on a machine without either tool,
# where it is the firewall.
test_switched_off_firewall_gets_no_iptables_rules() (
    load
    printf '[Service]
Environment=NEODESK_SUPERVISOR=systemd
' > "$SERVER_UNIT"
    printf 'CONSOLE_PORT=5000
HTTPS_ENABLED=false
' > "$CONSOLE_PATH/.env"
    systemctl() { return 3; }
    iptables() { printf '%s\n' "$*" >> "$TEST_DIR/iptables"; case "$1" in -C|-D) return 1 ;; esac; }
    iptables-save() { echo saved >> "$TEST_DIR/iptables"; }
    print_success() { :; }
    ufw() { [ "$1" != status ] || echo "Status: inactive"; }
    configure_firewall_rules > /dev/null 2>&1 || fail "the firewall step failed"
    close_console_firewall_ports > /dev/null 2>&1 || fail "closing the console's ports failed"
    if [ -s "$TEST_DIR/iptables" ]; then
        fail "with ufw switched off, neodesk.sh wrote iptables rules: $(tr '
' ';' < "$TEST_DIR/iptables")"
    fi
    unset -f ufw
    firewall-cmd() { :; }
    configure_firewall_rules > /dev/null 2>&1 || fail "the firewall step failed"
    if [ -s "$TEST_DIR/iptables" ]; then
        fail "with firewalld switched off, neodesk.sh wrote iptables rules: $(tr '
' ';' < "$TEST_DIR/iptables")"
    fi
    unset -f firewall-cmd
    if ! command -v ufw >/dev/null 2>&1 && ! command -v firewall-cmd >/dev/null 2>&1; then
        configure_firewall_rules > /dev/null 2>&1 || fail "the firewall step failed"
        grep -q -- '-A INPUT -p tcp --dport 21116 -j ACCEPT' "$TEST_DIR/iptables"             || fail "without ufw and firewalld, iptables got no rules: $(tr '
' ';' < "$TEST_DIR/iptables" 2>/dev/null)"
    fi
    rm -rf "$TEST_DIR"
)

# L225: --pg-uri (or POSTGRESQL_URI with PostgreSQL chosen) is the
# database as given: the install asks no database question and sets up no
# PostgreSQL of its own, which made a new address and dropped the given
# one. Without an address it sets PostgreSQL up on this machine.
test_pg_uri_is_used_as_given() (
    load
    local uri='postgres://bd:Given-Pass@db.example:5433/desk?sslmode=require'
    menu_choose() { echo menu >> "$TEST_DIR/calls"; MENU_CHOICE=1; }
    install_postgresql() { echo install_postgresql >> "$TEST_DIR/calls"; }
    setup_postgresql_database() { echo setup_postgresql_database >> "$TEST_DIR/calls"; POSTGRESQL_URI=postgres://made-here; }
    AUTO_MODE=false USE_POSTGRESQL=true POSTGRESQL_URI="$uri"
    choose_database_type > /dev/null
    [ "$USE_POSTGRESQL" = true ] || fail "the database question dropped the given PostgreSQL address"
    prepare_postgresql > /dev/null || fail "the PostgreSQL step failed"
    [ ! -s "$TEST_DIR/calls" ] || fail "with an address given the install still ran: $(tr '\n' ' ' < "$TEST_DIR/calls")"
    [ "$POSTGRESQL_URI" = "$uri" ] || fail "the given address became $POSTGRESQL_URI"
    POSTGRESQL_URI=""
    prepare_postgresql > /dev/null || fail "the PostgreSQL step without an address failed"
    [ "$(tr '\n' ' ' < "$TEST_DIR/calls")" = "install_postgresql setup_postgresql_database " ] \
        || fail "without an address PostgreSQL was not set up here: $(tr '\n' ' ' < "$TEST_DIR/calls" 2>/dev/null)"
    rm -rf "$TEST_DIR"
)

# L76: a self-signed certificate keeps the client API port on HTTP, which
# RustDesk clients accept; the console gets HTTPS.
test_self_signed_keeps_the_client_api_on_http() (
    load
    command -v openssl >/dev/null || { echo "skipped: no openssl"; exit 0; }
    get_public_ip() { echo 203.0.113.7; }
    deploy_ssl_material_to_rustdesk_dir() { return 0; }
    print_success() { :; }
    printf 'HTTPS_ENABLED=false\nSSL_CERT_PATH=\nSSL_KEY_PATH=\nRUSTDESK_API_TLS=auto\n' > "$CONSOLE_PATH/.env"
    generate_ssl_certificates >/dev/null 2>&1 || fail "no certificate was made"
    grep -qx 'HTTPS_ENABLED=true' "$CONSOLE_PATH/.env" || fail "HTTPS is not on"
    grep -qx 'RUSTDESK_API_TLS=false' "$CONSOLE_PATH/.env" || fail "the client API port takes the self-signed certificate: $(grep RUSTDESK_API_TLS "$CONSOLE_PATH/.env")"

    # L66: a binary with the cert command makes the pair, no OpenSSL needed
    rm -rf "$RUSTDESK_PATH/ssl"
    cat > "$RUSTDESK_PATH/neodesk-server" <<'FAKE'
#!/bin/bash
# Stands in for the binary: cert --out DIR [--host NAME]... (the commands it has)
[ "$1" = cert ] || exit 2
printf '%s\n' "$*" > "$3/args"
printf 'crt\n' > "$3/neodesk.crt"
printf 'key\n' > "$3/neodesk.key"
FAKE
    chmod +x "$RUSTDESK_PATH/neodesk-server"
    openssl() { echo "openssl was used" >> "$TEST_DIR/openssl"; command openssl "$@"; }
    generate_ssl_certificates >/dev/null 2>&1 || fail "the cert command made no certificate"
    [ ! -e "$TEST_DIR/openssl" ] || fail "OpenSSL made the certificate although the binary can"
    grep -q -- '--host 203.0.113.7' "$RUSTDESK_PATH/ssl/args" || fail "the cert command was not given the public address: $(cat "$RUSTDESK_PATH/ssl/args")"
    rm -rf "$TEST_DIR"
)

# L162: a port held by another program is named and left alone; only
# NeoDesk's own leftovers are stopped.
test_port_taken_by_another_program() (
    load
    command -v python3 >/dev/null && command -v ss >/dev/null || { echo "skipped: no python3 or ss"; exit 0; }
    printf 'SIGNAL_PORT=31116\nRELAY_PORT=31117\nCONSOLE_PORT=0\n' > "$CONSOLE_PATH/.env"
    python3 -m http.server 31116 --bind 127.0.0.1 >/dev/null 2>&1 &
    local other=$!
    local i
    for i in $(seq 1 50); do ss -ltnH "sport = :31116" | grep -q . && break; sleep 0.1; done
    local out
    kill_processes_holding_ports > "$TEST_DIR/out" 2>&1
    out=$(cat "$TEST_DIR/out")
    if ! kill -0 "$other" 2>/dev/null; then fail "the installer killed another program on port 31116"; fi
    kill "$other" 2>/dev/null; wait "$other" 2>/dev/null || true
    case "$out" in *31116*python3*) ;; *) fail "the port taken is not named with its program: $out" ;; esac
    [ -n "${FOREIGN_PORT_HOLDERS:-}" ] || fail "the install would go on with a port taken"
    rm -rf "$TEST_DIR"
)

# write_release DIR BRANCH: a release folder for BRANCH (main or dev) with
# two binaries, SHA256SUMS, and manifest.json as release-manifest writes it
# (json.MarshalIndent), its files out of order. RELEASE_COMMIT is the commit.
RELEASE_COMMIT=0123456789abcdef0123456789abcdef01234567
write_release() {
    local dir="$1" branch="$2" channel=stable amd arm
    [ "$branch" = dev ] && channel=development
    mkdir -p "$dir"
    [ -f "$dir/neodesk-server-linux-amd64" ] || printf 'the new amd64 server\n' > "$dir/neodesk-server-linux-amd64"
    printf 'the new arm64 server\n' > "$dir/neodesk-server-linux-arm64"
    amd=$(sha256sum "$dir/neodesk-server-linux-amd64" | cut -d' ' -f1)
    arm=$(sha256sum "$dir/neodesk-server-linux-arm64" | cut -d' ' -f1)
    cat > "$dir/manifest.json" <<JSON
{
  "schema_version": 1,
  "product": "neodesk-server",
  "version": "3.6.0",
  "channel": "$channel",
  "created": "2026-11-22T10:00:00Z",
  "owner": "neodeskapp",
  "repo": "neodesk",
  "branch": "$branch",
  "sha": "$RELEASE_COMMIT",
  "files": [
    {
      "path": "neodesk-server-linux-arm64",
      "sha256": "$arm",
      "size": 21,
      "os": "linux",
      "arch": "arm64"
    },
    {
      "path": "neodesk-server-linux-amd64",
      "sha256": "$amd",
      "size": 21,
      "os": "linux",
      "arch": "amd64"
    }
  ]
}
JSON
    printf '%s  neodesk-server-linux-amd64\n%s  neodesk-server-linux-arm64\n' "$amd" "$arm" > "$dir/SHA256SUMS"
}

# sign_release DIR KEY BRANCH: manifest.sig with KEY over the text the
# release workflow signs (updater.CanonicalPayload), written out here.
sign_release() {
    local dir="$1" amd arm
    amd=$(sha256sum "$dir/neodesk-server-linux-amd64" | cut -d' ' -f1)
    arm=$(sha256sum "$dir/neodesk-server-linux-arm64" | cut -d' ' -f1)
    printf 'neodesk-update-manifest-v1\nowner=neodeskapp\nrepo=neodesk\nbranch=%s\nsha=%s\nfiles=2\n%s  neodesk-server-linux-amd64\n%s  neodesk-server-linux-arm64\n' \
        "$3" "$RELEASE_COMMIT" "$amd" "$arm" > "$dir/.payload"
    openssl pkeyutl -sign -inkey "$2" -rawin -in "$dir/.payload" -out "$dir/.sig" 2>/dev/null || fail "openssl could not sign"
    { base64 -w0 "$dir/.sig"; echo; } > "$dir/manifest.sig"
    rm -f "$dir/.payload" "$dir/.sig"
}

# A152: a downloaded binary is installed only when the release's manifest
# carries a valid signature of the release key and names that binary for
# the channel. The installed neodesk-server checks it when it has
# verify-release, else openssl with the key in the script; an openssl that
# cannot check ed25519 is said plainly and the SHA-256 check stays.
test_release_signature() (
    load
    if ! command -v openssl >/dev/null || ! openssl pkeyutl -help 2>&1 | grep -q -- -rawin; then
        echo "skipped: no OpenSSL 3"
        exit 0
    fi
    local rel="$TEST_DIR/release" asset=neodesk-server-linux-amd64 out rc
    openssl genpkey -algorithm ed25519 -out "$TEST_DIR/key.pem" 2>/dev/null
    openssl genpkey -algorithm ed25519 -out "$TEST_DIR/other.pem" 2>/dev/null
    RELEASE_PUBLIC_KEY_PEM=$(openssl pkey -in "$TEST_DIR/key.pem" -pubout 2>/dev/null)
    print_error() { echo "$1"; }
    print_success() { echo "$1"; }
    # check BRANCH: verify_release_files' status and output
    check() { rc=0; out=$(verify_release_files "$rel" "$asset" "$1" 2>&1) || rc=$?; }

    write_release "$rel" main
    sign_release "$rel" "$TEST_DIR/key.pem" main
    check main
    [ "$rc" = 0 ] || fail "a release signed with the release key was refused ($rc): $out"
    case "$out" in *openssl*) ;; *) fail "the check does not say openssl checked it: $out" ;; esac
    check dev
    [ "$rc" = 1 ] || fail "a stable release passed for the development channel ($rc): $out"

    printf 'another server\n' > "$rel/$asset"
    check main
    [ "$rc" = 1 ] || fail "a binary the manifest does not name passed ($rc): $out"
    rm -f "$rel/$asset"
    write_release "$rel" main
    sign_release "$rel" "$TEST_DIR/key.pem" main
    sed -i "s/$RELEASE_COMMIT/fedcba9876543210fedcba9876543210fedcba98/" "$rel/manifest.json"
    check main
    [ "$rc" = 1 ] || fail "a manifest changed after signing passed ($rc): $out"
    write_release "$rel" main
    sign_release "$rel" "$TEST_DIR/other.pem" main
    check main
    [ "$rc" = 1 ] || fail "a release signed with another key passed ($rc): $out"
    rm -f "$rel/manifest.sig"
    check main
    [ "$rc" = 1 ] || fail "a release without manifest.sig passed ($rc): $out"
    write_release "$rel" dev
    sign_release "$rel" "$TEST_DIR/key.pem" dev
    check dev
    [ "$rc" = 0 ] || fail "a development release signed with the release key was refused ($rc): $out"

    # OpenSSL 1.1 checks no ed25519 signature over raw data
    openssl() { if [ "$1 ${2:-}" = "pkeyutl -help" ]; then echo "Usage: pkeyutl [options] -in file -inkey file"; else echo "OpenSSL 1.1.1w  11 Sep 2023"; fi; }
    rc=0
    verify_release_files "$rel" "$asset" dev > "$TEST_DIR/out" 2>&1 || rc=$?
    unset -f openssl
    [ "$rc" = 2 ] || fail "an openssl without ed25519 gave $rc: $(cat "$TEST_DIR/out")"
    case "$RELEASE_CHECK_PROBLEM" in *"OpenSSL 1.1.1w"*) ;; *) fail "the reason names no OpenSSL version: $RELEASE_CHECK_PROBLEM" ;; esac

    # An installed binary with verify-release checks the release itself,
    # with the release key it carries
    cat > "$RUSTDESK_PATH/neodesk-server" <<'FAKE'
#!/bin/sh
# Stands in for the binary: verify-release --manifest FILE (the commands it has)
printf '%s\n' "$*" > "$(dirname "$0")/verify-args"
if [ -f "$(dirname "$0")/refuse" ]; then
    echo "neodesk-server verify-release: the signature of the manifest does not verify against the release key" >&2
    exit 1
fi
echo "verified: neodesk-server 3.6.0 (0123456), $7"
FAKE
    chmod +x "$RUSTDESK_PATH/neodesk-server"
    sign_release "$rel" "$TEST_DIR/other.pem" dev
    check dev
    [ "$rc" = 0 ] || fail "the installed binary's check was not used ($rc): $out"
    [ "$(cat "$RUSTDESK_PATH/verify-args")" = "verify-release --manifest $rel/manifest.json --signature $rel/manifest.sig --name $asset $rel/$asset" ] \
        || fail "verify-release ran as: $(cat "$RUSTDESK_PATH/verify-args")"
    case "$out" in *"installed neodesk-server"*"3.6.0"*) ;; *) fail "the check does not say who checked it: $out" ;; esac
    check main
    [ "$rc" = 1 ] || fail "the installed binary's check let another channel's release pass ($rc): $out"
    touch "$RUSTDESK_PATH/refuse"
    check dev
    [ "$rc" = 1 ] || fail "a release the installed binary refused passed ($rc): $out"
    case "$out" in *"does not verify"*) ;; *) fail "the refusal does not say why: $out" ;; esac
    rm -rf "$TEST_DIR"
)

# A152: the download checks SHA256SUMS and the signature, installs nothing
# when one fails and says so; --skip-verify skips both.
test_download_checks_the_signature() (
    load
    if ! command -v openssl >/dev/null || ! openssl pkeyutl -help 2>&1 | grep -q -- -rawin; then
        echo "skipped: no OpenSSL 3"
        exit 0
    fi
    local rel="$TEST_DIR/release" out
    openssl genpkey -algorithm ed25519 -out "$TEST_DIR/key.pem" 2>/dev/null
    RELEASE_PUBLIC_KEY_PEM=$(openssl pkey -in "$TEST_DIR/key.pem" -pubout 2>/dev/null)
    print_info() { echo "$1"; }
    print_error() { echo "$1"; }
    print_warning() { echo "$1"; }
    release_asset_name() { echo neodesk-server-linux-amd64; }
    write_release "$rel" main
    sign_release "$rel" "$TEST_DIR/key.pem" main
    unset UPDATE_GITHUB_BRANCH_GIVEN RELEASE_BASE_URL
    UPDATE_GITHUB_BRANCH=main
    # curl: the stable release's files are the ones in $rel
    curl() {
        local a prev="" dest="" url="${!#}"
        for a in "$@"; do [ "$prev" = -o ] && dest="$a"; prev="$a"; done
        if [ -f "$rel/${url##*/}" ]; then cp "$rel/${url##*/}" "$dest"; printf '200'; else printf '404'; fi
    }
    mkdir -p "$TEST_DIR/stage"
    out=$(download_release_binary "$TEST_DIR/stage" 2>&1) || fail "a signed release was not installed: $out"
    [ -f "$TEST_DIR/stage/neodesk-server" ] || fail "the checked binary is not staged"
    case "$out" in *"SHA-256"*"signature checked"*) ;; *) fail "the download does not say what it checked: $out" ;; esac

    rm -rf "$TEST_DIR/stage" && mkdir -p "$TEST_DIR/stage"
    rm -f "$rel/manifest.sig"
    if out=$(download_release_binary "$TEST_DIR/stage" 2>&1); then fail "a release without a signature was installed: $out"; fi
    [ ! -f "$TEST_DIR/stage/neodesk-server" ] || fail "a binary without a signature is staged"
    case "$out" in *"no signed manifest"*"--skip-verify"*) ;; *) fail "the refusal does not say what to do: $out" ;; esac
    SKIP_VERIFY=true
    out=$(download_release_binary "$TEST_DIR/stage" 2>&1) || fail "--skip-verify did not install: $out"
    case "$out" in *"checks skipped (--skip-verify)"*) ;; *) fail "--skip-verify does not say what it skipped: $out" ;; esac
    rm -rf "$TEST_DIR"
)

# A152: an update of an install whose binary is older than the release key
# (no verify-release) goes through this installer's own download and
# check, from the repository .env names, instead of the old updater, which
# would take a release without checking its signature.
test_older_binary_updates_through_the_installer() (
    unset UPDATE_GITHUB_OWNER UPDATE_GITHUB_REPO
    load
    do_backup_silent() { :; }
    print_step() { :; }
    print_error() { :; }
    SERVER_BINARY_FILE=""
    FROM_SOURCE=false
    unset RELEASE_BASE_URL
    printf '#!/bin/sh\n# upgrade-env --from FILE (an older binary)\n' > "$RUSTDESK_PATH/neodesk-server"
    chmod +x "$RUSTDESK_PATH/neodesk-server"
    printf 'UPDATE_GITHUB_OWNER=acme\nUPDATE_GITHUB_REPO=desk\n' > "$CONSOLE_PATH/.env"
    server_cmd() { echo "server's updater" > "$TEST_DIR/seen"; return 1; }
    curl() { printf '%s\n' "${!#}" >> "$TEST_DIR/urls"; printf '000'; }
    update_go_install >/dev/null 2>&1 || true
    [ ! -e "$TEST_DIR/seen" ] || fail "the older binary's updater ran"
    grep -q '^https://github.com/acme/desk/releases/' "$TEST_DIR/urls" \
        || fail "the installer's download did not ask the repository of .env: $(cat "$TEST_DIR/urls" 2>/dev/null)"
    rm -rf "$TEST_DIR"
)

test_unit_environment
test_exec_args
test_write_server_unit
test_node_era_detection
test_no_upgrade_from_the_node_console
test_failed_update_puts_the_previous_binary_back
test_kill_stale_processes
test_fresh_env_admin_password
test_database_url_only
test_update_channel_in_env
test_update_channel_before_server_update
test_update_channel_kept_without_one
test_server_folder_mode
test_console_home_copies_no_console_files
test_releases_come_from_the_fork
test_update_keeps_the_admins_ports
test_channel_and_download_errors
test_one_command_install
test_installer_comes_from_the_release
test_update_moves_the_default_console_port
test_install_places_neodesk_setup_and_the_command_line
test_update_moves_the_old_manager
test_backups_move_to_the_neodesk_folder
test_no_settings_go_to_the_old_console_service
test_runs_without_term
test_apt_source_failure_goes_on
test_set_config_says_what_changed
test_protocol_flag_says_it_does_nothing
test_installer_uses_neodesk_names
test_reset_password_can_reset_two_factor
test_backup_uses_the_servers_backup
test_backups_are_for_root_only
test_old_backups_are_pruned
test_release_workflow_publishes_candidates_as_prereleases
test_release_workflow_takes_the_notes_file
test_no_stable_release_installs_the_newest_prerelease
test_no_stable_release_installs_dev
test_rerun_updates_and_forgets_the_password
test_panel_public_host
test_install_summary
test_password_box
test_interactive_install_asks_first
test_certificate_names_the_relay_address
test_fixed_relay_mode_is_said
test_postgresql_question
test_uninstall_says_what_stays
test_purge_handles_backups_and_the_database
test_update_without_a_new_release_changes_nothing
test_update_asks_the_fork
test_firewall_ports
test_reverse_proxy_closes_the_console_ports
test_inactive_ufw_is_left_alone
test_switched_off_firewall_gets_no_iptables_rules
test_pg_uri_is_used_as_given
test_self_signed_keeps_the_client_api_on_http
test_port_taken_by_another_program
test_release_signature
test_download_checks_the_signature
test_older_binary_updates_through_the_installer
echo "Go installer unit checks passed"
