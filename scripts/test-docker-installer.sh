#!/usr/bin/env bash
# Checks of neodesk-docker.sh without Docker: the manager's functions
# run in a shell of their own with the script's own settings (set -Eeuo
# pipefail), loaded without its main, and docker, curl, ss and id are
# stand-ins that keep their state in files of a temporary folder. At the
# end the checks of the image and compose files run
# (test-docker-entrypoint.sh).
#
#   bash scripts/test-docker-installer.sh
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

# load: a temporary folder with the install folder in it, and prelude.sh,
# which loads the manager's functions with Docker, the network and the
# clock replaced. The test's shell loads it too (for env_get, env_set).
load() {
    set --
    TEST_DIR=$(mktemp -d)
    : > "$TEST_DIR/calls"
    printf 'sha256:old\n' > "$TEST_DIR/good-image"
    printf 'sha256:old\n' > "$TEST_DIR/pulled"
    {
        printf 'NEODESK_DOCKER_SOURCED=1\nsource %q\n' "$SCRIPT_ROOT/install/neodesk-docker.sh"
        printf 'TEST_DIR=%q\nNEODESK_DIR=%q\nCALLS=%q\n' "$TEST_DIR" "$TEST_DIR/install" "$TEST_DIR/calls"
        printf 'HEALTH_TIMEOUT=4\nRED="" GREEN="" YELLOW="" CYAN="" BOLD="" NC=""\n'
        printf 'sleep() { :; }\nid() { printf "0\\n"; }\n'
        declare -f docker curl ss command_docker ufw firewall-cmd
    } > "$TEST_DIR/prelude.sh"
    # shellcheck source=/dev/null
    source "$TEST_DIR/prelude.sh"
    set +eu
}

# mgr CODE: runs CODE (manager functions, settings, stand-ins of its own)
# in a new shell, with the manager's set -Eeuo pipefail, as the script runs
mgr() {
    bash -c 'source "$1"; eval "$2"' mgr "$TEST_DIR/prelude.sh" "$1"
}

# docker: what the manager asks of Docker, recorded in $CALLS. State:
# exists, running (the container), current-image, pulled (the image the
# tag names), dir (the container's compose folder), engine (the Docker
# Engine's version, 29.1.0 without it), no-ipv6 (Docker can make no IPv6
# network).
docker() {
    printf '%s\n' "$*" >> "$CALLS"
    local arg format="" prev="" out="" file=""
    for arg in "$@"; do
        [ "$prev" = -f ] && format="$arg"
        [ "$prev" = -v ] && [[ "$arg" == *:/out ]] && out="${arg%:/out}"
        [[ "$arg" == /out/* ]] && file="${arg#/out/}"
        prev="$arg"
    done
    case "$1" in
        info) return 0 ;;
        version) cat "$TEST_DIR/engine" 2>/dev/null || printf '29.1.0\n'; return 0 ;;
        network)
            if [ "$2" = create ] && [ -f "$TEST_DIR/no-ipv6" ]; then
                printf 'Error response from daemon: could not find an available, non-overlapping IPv6 address pool\n' >&2
                return 1
            fi
            return 0 ;;
        compose)
            case "$2" in
                version) [ "${3:-}" = --short ] && printf '2.29.1\n'; return 0 ;;
                pull)
                    # missing-<tag>: ghcr.io has no image of that tag
                    local tag
                    tag=$(grep -E '^NEODESK_IMAGE_TAG=' "$NEODESK_DIR/.env" 2>/dev/null | tail -n 1 | cut -d= -f2)
                    if [ -f "$TEST_DIR/pull-fails" ] || [ -f "$TEST_DIR/missing-${tag:-latest}" ]; then printf 'manifest unknown
' >&2; return 1; fi
                    return 0 ;;
                up)
                    touch "$TEST_DIR/exists" "$TEST_DIR/running"
                    cp "$TEST_DIR/pulled" "$TEST_DIR/current-image" ;;
                stop|down) rm -f "$TEST_DIR/running" ;;
                start) touch "$TEST_DIR/running" ;;
                config) [ "${3:-}" = --hash ] && printf 'neodesk newhash\n' ;;
                run)
                    printf '%s\n' "${DATABASE_URL:-}" > "$TEST_DIR/run-env"
                    if [ -f "$TEST_DIR/move-fails" ]; then printf 'ERROR: the move broke\n' >&2; return 1; fi
                    printf 'Migration complete: 12 rows in 30 tables moved to PostgreSQL\n' ;;
            esac
            return 0 ;;
        container)
            if [ "${!#}" = neodesk-postgres ]; then
                [[ "$format" != *Health* ]] || printf 'healthy\n'
                return 0
            fi
            [ -f "$TEST_DIR/exists" ] && [ "${!#}" = neodesk ] || return 1
            case "$format" in
                *State.Running*) if [ -f "$TEST_DIR/running" ]; then printf 'true\n'; else printf 'false\n'; fi ;;
                *working_dir*) cat "$TEST_DIR/dir" 2>/dev/null || printf '%s\n' "$NEODESK_DIR" ;;
                *config-hash*) printf 'oldhash\n' ;;
                *Config.Image*) printf 'ghcr.io/neodeskapp/neodesk:latest\n' ;;
                *Image*) cat "$TEST_DIR/current-image" ;;
            esac
            return 0 ;;
        image)
            [ "$2" = inspect ] || return 0
            # image-env: the image's environment (one of this release
            # keeps its data in /opt/neodesk/data)
            case "$format" in
                *Config.Env*) cat "$TEST_DIR/image-env" 2>/dev/null || printf 'RUSTDESK_PATH=/opt/neodesk/data\n' ;;
                ?*) cat "$TEST_DIR/pulled" ;;
            esac
            return 0 ;;
        exec)
            case "$*" in
                *-version*) printf 'neodesk-server 3.5.270 (built test)\n' ;;
                *.admin_credentials*)
                    [ ! -f "$TEST_DIR/first-password" ] || printf 'Admin Username: admin\nAdmin Password: %s\n' "$(cat "$TEST_DIR/first-password")" ;;
                *id_ed25519.pub*) printf 'PUBKEYbase64=\n' ;;
                *reset-password*) cat > /dev/null ;;
            esac
            return 0 ;;
        run)
            [ -z "$out" ] || [ -z "$file" ] || : > "$out/$file"
            return 0 ;;
        tag) cut -d' ' -f1 <<< "$2" > "$TEST_DIR/pulled"; return 0 ;;
        volume) return 1 ;;
        logs) printf 'the log\n'; return 0 ;;
    esac
    return 0
}

# curl: ghcr.io answers, the public address is 203.0.113.10, and the health
# URLs answer while the container runs the good image
curl() {
    local url="${!#}"
    case "$url" in
        *ghcr.io/v2/*) return 0 ;;
        *checkip*|*ipify*) printf '203.0.113.10\n' ;;
        *health*)
            [ -f "$TEST_DIR/running" ] && [ "$(cat "$TEST_DIR/current-image" 2>/dev/null)" = "$(cat "$TEST_DIR/good-image")" ] ;;
        *) return 1 ;;
    esac
}

# ss: a port is taken when $TEST_DIR/taken-<port> holds the program's name
ss() {
    local port="${2##*:}"
    [ -f "$TEST_DIR/taken-$port" ] || return 0
    printf 'LISTEN 0 4096 0.0.0.0:%s 0.0.0.0:* users:(("%s",pid=7,fd=3))\n' "$port" "$(cat "$TEST_DIR/taken-$port")"
}

# ufw, firewall-cmd: the host firewall, on when $TEST_DIR/fw says ufw or
# firewalld. Its rules are in $TEST_DIR/fw-rules (ufw: as "ufw status"
# lists them, IPv4 and IPv6; firewalld: one port per line), the calls in
# $CALLS.
ufw() {
    printf 'ufw %s\n' "$*" >> "$CALLS"
    local rules="$TEST_DIR/fw-rules"
    case "$1" in
        status)
            if [ "$(cat "$TEST_DIR/fw" 2>/dev/null)" = ufw ]; then
                printf 'Status: active\n\nTo                         Action      From\n'
                cat "$rules" 2>/dev/null || true
            else
                printf 'Status: inactive\n'
            fi ;;
        allow)
            printf '%-26s ALLOW       Anywhere                   # %s\n' "$2" "${4:-}" >> "$rules"
            printf '%-26s ALLOW       Anywhere (v6)              # %s\n' "$2 (v6)" "${4:-}" >> "$rules" ;;
        delete)
            grep -vE "^$3 " "$rules" > "$rules.tmp" || true
            mv "$rules.tmp" "$rules" ;;
    esac
    return 0
}
firewall-cmd() {
    printf 'firewall-cmd %s\n' "$*" >> "$CALLS"
    local rules="$TEST_DIR/fw-rules" arg
    for arg in "$@"; do
        case "$arg" in
            --state)
                if [ "$(cat "$TEST_DIR/fw" 2>/dev/null)" = firewalld ]; then printf 'running\n'; else printf 'not running\n'; return 252; fi ;;
            --query-port=*) grep -qx "${arg#*=}" "$rules" 2>/dev/null; return ;;
            --add-port=*) printf '%s\n' "${arg#*=}" >> "$rules" ;;
            --remove-port=*) grep -vx "${arg#*=}" "$rules" > "$rules.tmp" || true; mv "$rules.tmp" "$rules" ;;
        esac
    done
    return 0
}

# The stand-in for Docker, callable from a test's own docker()
eval "command_docker() $(declare -f docker | tail -n +2)"

# A fresh install writes the settings and the release's compose file,
# starts the container, waits for it and prints the console address, the
# first password once and the client settings. No file of the install
# folder holds the password, and running the script again prints none.
test_fresh_install() (
    load
    printf 'first-pass-0123\n' > "$TEST_DIR/first-password"
    local out
    out=$(mgr 'COMMAND=install; do_install' 2>&1) || fail "a fresh install failed: $out"
    grep -qx 'RELAY_SERVERS=203.0.113.10' "$NEODESK_DIR/.env" || fail ".env has no relay address"
    cmp -s "$NEODESK_DIR/docker-compose.yml" "$SCRIPT_ROOT/install/docker-compose.quick.single.yml" || fail "the compose file is not the release's"
    grep -qx 'compose up -d --remove-orphans' "$CALLS" || fail "the container was not started: $(cat "$CALLS")"
    printf '%s' "$out" | grep -q 'Web console: *http://203.0.113.10:3375' || fail "no console address in: $out"
    printf '%s' "$out" | grep -q 'ID server: *203.0.113.10' || fail "no ID server in: $out"
    printf '%s' "$out" | grep -q 'Relay server: *203.0.113.10:21117$' || fail "the relay is not given with its port: $out"
    # DOCKER-QUICK-8: the hints name the manager by its full path, and the
    # install folder has the copy they name
    [ -x "$NEODESK_DIR/neodesk-docker.sh" ] || fail "the install folder has no copy of the manager"
    printf '%s' "$out" | grep -qF "Update:          sudo bash $NEODESK_DIR/neodesk-docker.sh --dir $NEODESK_DIR " \
        || fail "the update hint does not name the manager by its full path: $out"
    printf '%s' "$out" | grep -q 'Key: *PUBKEYbase64=' || fail "no key in: $out"
    printf '%s' "$out" | grep -q 'docker compose logs -f neodesk' || fail "no logs hint in: $out"
    [ "$(printf '%s' "$out" | grep -c 'first-pass-0123')" = 1 ] || fail "the first password is not printed exactly once: $out"
    if grep -rq 'first-pass-0123' "$NEODESK_DIR"; then fail "a file in the install folder holds the password"; fi

    out=$(mgr 'COMMAND=install; do_install' 2>&1) || fail "the second run failed: $out"
    if printf '%s' "$out" | grep -q 'first-pass-0123'; then fail "the second run printed the password again"; fi
    printf '%s' "$out" | grep -qF "a new admin password: sudo bash $NEODESK_DIR/neodesk-docker.sh --dir $NEODESK_DIR reset-password" \
        || fail "the password hint does not name the manager by its full path: $out"
    if printf '%s' "$out" | grep -qE '(^|[^/])neodesk-docker\.sh (update|reset-password|--adopt)'; then
        fail "a hint names the manager without its folder: $out"
    fi
    rm -rf "$TEST_DIR"
)

# L254: before any release, ghcr.io has no latest image. Without a tag
# given or saved the install runs the development image dev and says so,
# as the native installers take the development build; a tag given stays
# and fails with what to do.
test_no_release_image_runs_dev() (
    load
    : > "$TEST_DIR/missing-latest"
    local out
    out=$(mgr 'COMMAND=install; do_install' 2>&1) || fail "an install without a release image failed: $out"
    grep -qx 'NEODESK_IMAGE_TAG=dev' "$NEODESK_DIR/.env" || fail "the install does not run dev: $(cat "$NEODESK_DIR/.env")"
    printf '%s' "$out" | grep -q 'no release image yet' || fail "the install does not say it took the development image: $out"
    rm -rf "$NEODESK_DIR"
    if out=$(mgr 'NEODESK_IMAGE_TAG_SET=latest; COMMAND=install; do_install' 2>&1); then fail "an install with latest given ran another image"; fi
    printf '%s' "$out" | grep -q 'was not found on ghcr.io' || fail "a missing given tag is not said: $out"
    rm -rf "$TEST_DIR"
)

# An update backs the volumes up, with the container stopped, before the
# new container starts; when the new image does not get healthy, the
# previous one runs again.
test_update_goes_back_when_unhealthy() (
    load
    mgr 'COMMAND=install; do_install' > /dev/null 2>&1 || fail "the install failed"
    : > "$CALLS"
    local out stop_line backup_line
    if out=$(mgr 'docker() {
            if [ "$1" = compose ] && [ "$2" = pull ]; then
                printf "%s\n" "$*" >> "$CALLS"
                printf "sha256:new\n" > "$TEST_DIR/pulled"
                return 0
            fi
            command_docker "$@"
        }
        COMMAND=update; do_update' 2>&1); then fail "an update to an unhealthy image reported success: $out"; fi
    printf '%s' "$out" | grep -q 'runs the previous version again' || fail "no word of the way back: $out"
    grep -q '^tag sha256:old ' "$CALLS" || fail "the previous image was not tagged again: $(cat "$CALLS")"
    grep -q 'compose up -d --force-recreate neodesk' "$CALLS" || fail "the container was not made again from the previous image"
    [ "$(cat "$TEST_DIR/current-image")" = sha256:old ] || fail "the container does not run the previous image"
    ls "$NEODESK_DIR"/backups/neodesk-*.tar.gz > /dev/null 2>&1 || fail "no backup before the update"
    stop_line=$(grep -n 'compose stop' "$CALLS" | head -n 1 | cut -d: -f1)
    backup_line=$(grep -n '^run .*tar' "$CALLS" | head -n 1 | cut -d: -f1)
    { [ -n "$stop_line" ] && [ -n "$backup_line" ] && [ "$stop_line" -lt "$backup_line" ]; } \
        || fail "the backup was not taken with the container stopped"
    rm -rf "$TEST_DIR"
)

# L460: an update's backup holds .env and docker-compose.yml as they were
# before the update, not the files it writes. When the new container does
# not get healthy, the way back puts those files back and gives the
# previous image its own tag again, not the new release's.
test_update_backs_up_the_files_from_before() (
    load
    mgr 'NEODESK_IMAGE_TAG_SET=3.6.0-rc.3; COMMAND=install; do_install' > /dev/null 2>&1 || fail "the install failed"
    # The compose file of the release before, as its manager wrote it
    printf '# the compose file of 3.6.0-rc.3\n' >> "$NEODESK_DIR/docker-compose.yml"
    sha256sum "$NEODESK_DIR/docker-compose.yml" | cut -d' ' -f1 > "$NEODESK_DIR/.compose.sha256"
    cp "$NEODESK_DIR/.env" "$TEST_DIR/env-before"
    cp "$NEODESK_DIR/docker-compose.yml" "$TEST_DIR/compose-before"
    # docker: the pull brings sha256:new; the container runs 3.6.0-rc.3
    local update='docker() {
            case "$1 ${2:-}" in
                "compose pull")
                    printf "%s\n" "$*" >> "$CALLS"
                    printf "sha256:new\n" > "$TEST_DIR/pulled"
                    return 0 ;;
                "container inspect")
                    case "$*" in *Config.Image*) printf "ghcr.io/neodeskapp/neodesk:3.6.0-rc.3\n"; return 0 ;; esac ;;
            esac
            command_docker "$@"
        }
        NEODESK_IMAGE_TAG_SET=3.6.0-rc.4; COMMAND=update; do_update'
    : > "$CALLS"
    local out backup
    if out=$(mgr "$update" 2>&1); then fail "an update to an unhealthy image reported success: $out"; fi
    backup=$(ls "$NEODESK_DIR"/backups/neodesk-*.env 2>/dev/null | head -n 1)
    [ -n "$backup" ] || fail "the backup has no .env"
    cmp -s "$backup" "$TEST_DIR/env-before" \
        || fail "the backup's .env is not the one from before the update: $(grep IMAGE_TAG "$backup")"
    cmp -s "${backup%.env}.docker-compose.yml" "$TEST_DIR/compose-before" \
        || fail "the backup's docker-compose.yml is not the one from before the update"
    grep -qx 'tag sha256:old ghcr.io/neodeskapp/neodesk:3.6.0-rc.3' "$CALLS" \
        || fail "the previous image did not get its own tag back: $(grep '^tag' "$CALLS")"
    if grep -q '^tag .*3\.6\.0-rc\.4' "$CALLS"; then fail "the previous image was tagged as the new release: $(grep '^tag' "$CALLS")"; fi
    cmp -s "$NEODESK_DIR/.env" "$TEST_DIR/env-before" \
        || fail "the way back did not put .env back: $(grep IMAGE_TAG "$NEODESK_DIR/.env")"
    cmp -s "$NEODESK_DIR/docker-compose.yml" "$TEST_DIR/compose-before" || fail "the way back did not put docker-compose.yml back"
    printf '%s' "$out" | grep -q 'runs the previous version again' || fail "no word of the way back: $out"

    # A good update keeps the files from before in its backup too
    rm -f "$NEODESK_DIR"/backups/neodesk-*
    printf 'sha256:new\n' > "$TEST_DIR/good-image"
    out=$(mgr "$update" 2>&1) || fail "the update failed: $out"
    backup=$(ls "$NEODESK_DIR"/backups/neodesk-*.env 2>/dev/null | head -n 1)
    [ -n "$backup" ] && cmp -s "$backup" "$TEST_DIR/env-before" \
        || fail "the backup of a good update has another .env: $(grep IMAGE_TAG "$backup")"
    cmp -s "${backup%.env}.docker-compose.yml" "$TEST_DIR/compose-before" \
        || fail "the backup of a good update has another docker-compose.yml"
    grep -qx 'NEODESK_IMAGE_TAG=3.6.0-rc.4' "$NEODESK_DIR/.env" || fail "the update did not keep the new tag: $(cat "$NEODESK_DIR/.env")"
    rm -rf "$TEST_DIR"
)

# L446: the data volume is mounted at /opt/neodesk/data, where the image
# keeps its data; no line of the compose files uses /opt/rustdesk.
test_data_volume_at_the_neodesk_path() (
    load
    mgr 'COMMAND=install; do_install' > /dev/null 2>&1 || fail "the install failed"
    grep -qF -- '- neodesk-data:${NEODESK_DATA_PATH:-/opt/neodesk/data}' "$NEODESK_DIR/docker-compose.yml" \
        || fail "the compose file mounts the data volume elsewhere: $(grep 'neodesk-data:' "$NEODESK_DIR/docker-compose.yml")"
    local f
    for f in install/docker-compose.quick.single.yml install/docker-compose.quick.single.macvlan.yml install/docker-compose.single.yml; do
        if grep -nE '^[^#]*/opt/rustdesk' "$SCRIPT_ROOT/$f"; then fail "$f uses /opt/rustdesk"; fi
    done
    if grep -q '^NEODESK_DATA_PATH=' "$NEODESK_DIR/.env"; then fail "an image of this release got NEODESK_DATA_PATH"; fi
    rm -rf "$TEST_DIR"
)

# L446: an update of an install from before (3.6.0-rc.4: the data volume
# at /opt/rustdesk) gets the release's compose file, with the same volume
# at /opt/neodesk/data. The image has nothing at /opt/rustdesk any more, so
# the update rewrites the old path where the install names it: in .env, in
# a compose file changed by hand (which stays, with the user's changes) and
# in docker-compose.override.yml, where it becomes the data path the
# release's compose file mounts at. The files as they were are in the
# update's backup, and a failed update puts them back. An image pinned from
# before keeps its data path (NEODESK_DATA_PATH=/opt/rustdesk) and its
# files, and an image of this release takes the setting away again.
test_update_from_the_old_data_path() (
    load
    mgr 'NEODESK_IMAGE_TAG_SET=3.6.0-rc.4; COMMAND=install; do_install' > /dev/null 2>&1 || fail "the install failed"
    # The compose file of 3.6.0-rc.4, as its manager wrote it
    sed -e 's|${NEODESK_DATA_PATH:-/opt/neodesk/data}|/opt/rustdesk|g' "$SCRIPT_ROOT/install/docker-compose.quick.single.yml" \
        > "$NEODESK_DIR/docker-compose.yml"
    sha256sum "$NEODESK_DIR/docker-compose.yml" | cut -d' ' -f1 > "$NEODESK_DIR/.compose.sha256"
    printf 'SSL_CERT_PATH=/opt/rustdesk/ssl/console.pem\n' >> "$NEODESK_DIR/.env"
    printf 'services:\n  neodesk:\n    volumes:\n      - /srv/neodesk:/opt/rustdesk\n' > "$NEODESK_DIR/docker-compose.override.yml"
    cp "$NEODESK_DIR/.env" "$TEST_DIR/env-before"
    cp "$NEODESK_DIR/docker-compose.override.yml" "$TEST_DIR/override-before"
    printf 'sha256:new\n' > "$TEST_DIR/good-image"
    local pull='docker() {
            if [ "$1 ${2:-}" = "compose pull" ]; then printf "sha256:new\n" > "$TEST_DIR/pulled"; return 0; fi
            command_docker "$@"
        }'
    local out backup
    out=$(mgr "$pull; NEODESK_IMAGE_TAG_SET=latest; COMMAND=update; do_update" 2>&1) || fail "the update failed: $out"
    cmp -s "$NEODESK_DIR/docker-compose.yml" "$SCRIPT_ROOT/install/docker-compose.quick.single.yml" || fail "the update kept the old compose file"
    grep -qx 'SSL_CERT_PATH=/opt/neodesk/data/ssl/console.pem' "$NEODESK_DIR/.env" \
        || fail "a setting that names the old path was not rewritten: $(grep SSL_CERT_PATH "$NEODESK_DIR/.env")"
    grep -qxF '      - /srv/neodesk:${NEODESK_DATA_PATH:-/opt/neodesk/data}' "$NEODESK_DIR/docker-compose.override.yml" \
        || fail "docker-compose.override.yml still mounts at the old path: $(cat "$NEODESK_DIR/docker-compose.override.yml")"
    if grep -q '^NEODESK_DATA_PATH=' "$NEODESK_DIR/.env"; then fail "an image of this release got NEODESK_DATA_PATH"; fi
    printf '%s' "$out" | grep -q '/opt/neodesk/data' || fail "the update does not say it rewrote the old path: $out"
    backup=$(ls -1t "$NEODESK_DIR"/backups/neodesk-*.env 2>/dev/null | head -n 1)
    cmp -s "$backup" "$TEST_DIR/env-before" || fail "the update's backup does not hold .env as it was"
    cmp -s "${backup%.env}.docker-compose.override.yml" "$TEST_DIR/override-before" \
        || fail "the update's backup does not hold docker-compose.override.yml as it was"

    # A compose file of 3.6.0-rc.4 changed by hand: the user's change stays,
    # the old path goes
    sed -e 's|${NEODESK_DATA_PATH:-/opt/neodesk/data}|/opt/rustdesk|g' "$SCRIPT_ROOT/install/docker-compose.quick.single.yml" \
        > "$NEODESK_DIR/docker-compose.yml"
    printf '# my change\n' >> "$NEODESK_DIR/docker-compose.yml"
    cp "$NEODESK_DIR/docker-compose.yml" "$TEST_DIR/compose-before"
    printf 'sha256:newer\n' > "$TEST_DIR/good-image"
    out=$(mgr 'docker() {
            if [ "$1 ${2:-}" = "compose pull" ]; then printf "sha256:newer\n" > "$TEST_DIR/pulled"; return 0; fi
            command_docker "$@"
        }
        NEODESK_IMAGE_TAG_SET=latest; COMMAND=update; do_update' 2>&1) || fail "the update of a changed compose file failed: $out"
    grep -q '^# my change' "$NEODESK_DIR/docker-compose.yml" || fail "the update dropped the change by hand"
    if grep -nE '^[^#]*/opt/rustdesk' "$NEODESK_DIR/docker-compose.yml"; then fail "the compose file changed by hand still uses the old path"; fi
    grep -qF -- '- neodesk-data:${NEODESK_DATA_PATH:-/opt/neodesk/data}' "$NEODESK_DIR/docker-compose.yml" \
        || fail "the compose file changed by hand mounts the data volume at: $(grep 'neodesk-data:' "$NEODESK_DIR/docker-compose.yml")"
    backup=$(ls -1t "$NEODESK_DIR"/backups/neodesk-*.docker-compose.yml 2>/dev/null | head -n 1)
    cmp -s "$backup" "$TEST_DIR/compose-before" || fail "the update's backup does not hold the changed compose file as it was"

    # A failed update puts the files with the old path back, for the image
    # from before
    cp "$TEST_DIR/compose-before" "$NEODESK_DIR/docker-compose.yml"
    cp "$TEST_DIR/env-before" "$NEODESK_DIR/.env"
    cp "$TEST_DIR/override-before" "$NEODESK_DIR/docker-compose.override.yml"
    if out=$(mgr 'docker() {
            if [ "$1 ${2:-}" = "compose pull" ]; then printf "sha256:broken\n" > "$TEST_DIR/pulled"; return 0; fi
            command_docker "$@"
        }
        NEODESK_IMAGE_TAG_SET=latest; COMMAND=update; do_update' 2>&1); then fail "an update to an unhealthy image reported success: $out"; fi
    cmp -s "$NEODESK_DIR/.env" "$TEST_DIR/env-before" || fail "the way back did not put .env back"
    cmp -s "$NEODESK_DIR/docker-compose.yml" "$TEST_DIR/compose-before" || fail "the way back did not put docker-compose.yml back"
    cmp -s "$NEODESK_DIR/docker-compose.override.yml" "$TEST_DIR/override-before" \
        || fail "the way back did not put docker-compose.override.yml back"

    # An image pinned from before the rename keeps its data where it looks
    rm -f "$NEODESK_DIR/docker-compose.override.yml"
    printf 'PATH=/usr/bin\nRUSTDESK_PATH=/opt/rustdesk\n' > "$TEST_DIR/image-env"
    printf 'sha256:old\n' > "$TEST_DIR/good-image"
    out=$(mgr 'docker() {
            if [ "$1 ${2:-}" = "compose pull" ]; then printf "sha256:old\n" > "$TEST_DIR/pulled"; return 0; fi
            command_docker "$@"
        }
        NEODESK_IMAGE_TAG_SET=3.6.0-rc.4; COMMAND=update; do_update' 2>&1) || fail "the update to a pinned older image failed: $out"
    grep -qx 'NEODESK_DATA_PATH=/opt/rustdesk' "$NEODESK_DIR/.env" \
        || fail "an older image's data path is not in .env: $(cat "$NEODESK_DIR/.env")"
    grep -qx 'SSL_CERT_PATH=/opt/rustdesk/ssl/console.pem' "$NEODESK_DIR/.env" \
        || fail "a setting for an older image was rewritten: $(grep SSL_CERT_PATH "$NEODESK_DIR/.env")"
    rm -f "$TEST_DIR/image-env"
    printf 'sha256:new\n' > "$TEST_DIR/good-image"
    out=$(mgr "$pull; NEODESK_IMAGE_TAG_SET=latest; COMMAND=update; do_update" 2>&1) || fail "the update back to this release failed: $out"
    if grep -q '^NEODESK_DATA_PATH=' "$NEODESK_DIR/.env"; then fail "NEODESK_DATA_PATH stayed with an image of this release"; fi
    rm -rf "$TEST_DIR"
)

# L446: a backup holds the folders neodesk (the data volume) and console;
# one from before the rename (rustdesk and console) goes back into the same
# volumes.
test_old_backup_restores() (
    load
    mgr 'COMMAND=install; do_install' > /dev/null 2>&1 || fail "the install failed"
    : > "$CALLS"
    mgr 'backup_volumes' > /dev/null 2>&1 || fail "the backup failed"
    grep -q '^run .*-v neodesk-data:/backup/neodesk:ro .*czf /out/neodesk-[0-9-]*\.tar\.gz -C /backup neodesk console' "$CALLS" \
        || fail "a backup does not hold the folders neodesk and console: $(grep '^run' "$CALLS")"
    : > "$CALLS"
    mgr "restore_volumes '$NEODESK_DIR/backups/neodesk-20260101-000000.tar.gz'" > /dev/null 2>&1 || fail "the restore failed"
    grep -q '^run .*-v neodesk-data:/backup/neodesk .*-v neodesk-console-data:/backup/console .*tar xzf' "$CALLS" \
        || fail "the restore does not put a backup's folder neodesk into the data volume: $(cat "$CALLS")"
    grep -q '^run .*-v neodesk-data:/backup/rustdesk .*tar xzf' "$CALLS" \
        || fail "the restore does not put an older backup's folder rustdesk into the data volume: $(cat "$CALLS")"
    rm -rf "$TEST_DIR"
)

# L125: a compose file changed by hand stays on update; the release's file
# goes next to it.
test_update_keeps_compose_edits() (
    load
    mgr 'COMMAND=install; do_install' > /dev/null 2>&1 || fail "the install failed"
    printf '# my change\n' >> "$NEODESK_DIR/docker-compose.yml"
    mgr 'write_compose_file' 2> /dev/null
    grep -q '^# my change' "$NEODESK_DIR/docker-compose.yml" || fail "the update overwrote a changed compose file"
    cmp -s "$NEODESK_DIR/docker-compose.yml.new" "$SCRIPT_ROOT/install/docker-compose.quick.single.yml" \
        || fail "the release's compose file is not next to it"
    rm -rf "$TEST_DIR"
)

# L125: a new install with --postgres runs on PostgreSQL: the settings name
# it, and the made-up password is printed nowhere.
test_fresh_install_on_postgres() (
    load
    local out pass
    out=$(mgr 'USE_POSTGRES=true; COMMAND=install; do_install' 2>&1) || fail "a PostgreSQL install failed: $out"
    pass=$(env_get POSTGRES_PASSWORD)
    [ "${#pass}" -ge 32 ] || fail "no PostgreSQL password made"
    [ "$(env_get DATABASE_URL)" = "postgres://neodesk:${pass}@postgres:5432/neodesk?sslmode=disable" ] \
        || fail "DATABASE_URL is $(env_get DATABASE_URL)"
    [ "$(env_get COMPOSE_PROFILES)" = postgres ] || fail "the postgres service is not on"
    # L70: the address alone names the database
    if grep -q '^DB_TYPE=' "$NEODESK_DIR/.env"; then fail "the settings name the database type besides DATABASE_URL"; fi
    if printf '%s' "$out" | grep -qF "$pass"; then fail "the PostgreSQL password was printed"; fi
    grep -qE '^  postgres:' "$NEODESK_DIR/docker-compose.yml" || fail "the compose file has no postgres service"
    rm -rf "$TEST_DIR"
)

# NeoDesk from another install (an older manager, a split install) is
# not touched: the error says how to take it over.
test_foreign_install_is_left_alone() (
    load
    touch "$TEST_DIR/exists" "$TEST_DIR/running"
    printf '/root/old-neodesk\n' > "$TEST_DIR/dir"
    local out
    if out=$(mgr 'COMMAND=install; do_install' 2>&1); then fail "an install over another install's container went on"; fi
    printf '%s' "$out" | grep -q -- '--adopt' || fail "the error does not name --adopt: $out"
    [ ! -e "$NEODESK_DIR/.env" ] || fail "the install folder was written although another install runs"
    if grep -qE '^compose (up|stop|down)|^stop |^rm ' "$CALLS"; then fail "the other install's container was touched"; fi
    rm -rf "$TEST_DIR"
)

# Errors say what to do: a port taken, Compose v1 only, no Docker, no
# sudo, no internet.
test_errors_say_what_to_do() (
    load
    local out
    printf 'python3\n' > "$TEST_DIR/taken-3375"
    if out=$(mgr 'COMMAND=install; do_install' 2>&1); then fail "an install with port 3375 taken went on"; fi
    printf '%s' "$out" | grep -q 'TCP 3375: python3' || fail "the port error does not name the program: $out"
    printf '%s' "$out" | grep -q -- '--console-port' || fail "the port error does not name --console-port: $out"
    rm -f "$TEST_DIR/taken-3375"
    printf 'hbbs\n' > "$TEST_DIR/taken-21116"
    if out=$(mgr 'COMMAND=install; do_install' 2>&1); then fail "an install with 21116 taken went on"; fi
    printf '%s' "$out" | grep -q 'RustDesk server' || fail "the error does not name the RustDesk server: $out"
    printf '%s' "$out" | grep -q -- '--import-keys' || fail "the RustDesk error does not say how to keep its key: $out"
    rm -f "$TEST_DIR/taken-21116"
    # L225: the compose file publishes the console's HTTPS port too
    printf 'nginx\n' > "$TEST_DIR/taken-5443"
    if out=$(mgr 'COMMAND=install; do_install' 2>&1); then fail "an install with 5443 taken went on"; fi
    printf '%s' "$out" | grep -q 'TCP 5443: nginx' || fail "the port error does not name 5443: $out"
    rm -f "$TEST_DIR/taken-5443"

    if out=$(mgr 'docker() { case "$1" in info) return 0 ;; *) return 1 ;; esac; }
              docker-compose() { :; }
              check_docker' 2>&1); then fail "Compose v1 alone passed the check"; fi
    printf '%s' "$out" | grep -q 'docker-compose-plugin' || fail "the Compose error does not say how to install v2: $out"

    if out=$(mgr 'unset -f docker; PATH="$TEST_DIR/empty"; INSTALL_DOCKER=false; check_docker' 2>&1); then
        fail "a missing Docker passed the check"
    fi
    printf '%s' "$out" | grep -q 'get.docker.com' || fail "the Docker error does not say how to install it: $out"

    if out=$(mgr 'id() { printf "1000\n"; }; docker() { return 1; }; need_root_or_docker' 2>&1); then
        fail "a user without root went on"
    fi
    printf '%s' "$out" | grep -q 'sudo' || fail "the root error does not name sudo: $out"

    if out=$(mgr 'curl() { return 6; }; check_internet' 2>&1); then fail "no internet passed the check"; fi
    printf '%s' "$out" | grep -q -- '--no-pull' || fail "the internet error does not say what to do: $out"
    rm -rf "$TEST_DIR"
)

# Uninstall says what it leaves; --purge without --yes and without a
# terminal deletes nothing.
test_uninstall_says_what_it_leaves() (
    load
    mgr 'COMMAND=install; do_install' > /dev/null 2>&1 || fail "the install failed"
    local out
    out=$(mgr 'do_uninstall' 2>&1) || fail "uninstall failed: $out"
    printf '%s' "$out" | grep -q 'neodesk-data' || fail "uninstall does not name the volumes it leaves: $out"
    printf '%s' "$out" | grep -q 'docker image rm' || fail "uninstall does not name the image it leaves: $out"
    if grep -q 'down -v' "$CALLS"; then fail "uninstall without --purge deleted the volumes"; fi
    if out=$(mgr 'PURGE=true; do_uninstall' 2>&1 < /dev/null); then fail "--purge without --yes went on"; fi
    if grep -q 'down -v' "$CALLS"; then fail "--purge without --yes deleted the volumes"; fi
    rm -rf "$TEST_DIR"
)

# L206: --purge of an install on PostgreSQL dumps the database before the
# volumes go: only NeoDesk stops for the backup, the postgres service
# keeps running for pg_dump.
test_purge_dumps_postgres_first() (
    load
    mgr 'USE_POSTGRES=true; COMMAND=install; do_install' > /dev/null 2>&1 || fail "the PostgreSQL install failed"
    : > "$CALLS"
    local out stop dump down
    out=$(mgr 'PURGE=true; ASSUME_YES=true; do_uninstall' 2>&1) || fail "--purge --yes failed: $out"
    if grep -qx 'compose stop -t 30' "$CALLS"; then fail "--purge stopped the postgres service before its dump: $(cat "$CALLS")"; fi
    stop=$(grep -n '^compose stop -t 30 neodesk$' "$CALLS" | head -n 1 | cut -d: -f1)
    dump=$(grep -n '^compose exec -T postgres pg_dump' "$CALLS" | head -n 1 | cut -d: -f1)
    down=$(grep -n 'down -v' "$CALLS" | head -n 1 | cut -d: -f1)
    { [ -n "$stop" ] && [ -n "$dump" ] && [ -n "$down" ] && [ "$stop" -lt "$dump" ] && [ "$dump" -lt "$down" ]; } \
        || fail "--purge did not stop NeoDesk, dump PostgreSQL and then delete the volumes, in that order: $(cat "$CALLS")"
    rm -rf "$TEST_DIR"
)

# --help names every option the script takes.
test_help_lists_every_option() (
    local opt help
    help=$(bash "$SCRIPT_ROOT/install/neodesk-docker.sh" --help)
    for opt in $(sed -n '/^parse_args() {/,/^}/p' "$SCRIPT_ROOT/install/neodesk-docker.sh" | grep -oE '^ +--[a-z-]+' | tr -d ' ' | sort -u); do
        printf '%s' "$help" | grep -q -- "$opt" || fail "--help does not list $opt"
    done
)

# L43: the switch to PostgreSQL stops NeoDesk, backs the volumes up,
# starts the postgres service, moves the data with the server's sqlite2pg
# and starts NeoDesk on PostgreSQL. The database address reaches the
# tool through the environment, never the command line.
test_postgres_switch_moves_the_data() (
    load
    mgr 'COMMAND=install; do_install' > /dev/null 2>&1 || fail "the install failed"
    env_set COMPOSE_PROFILES guacd
    # An older .env names its type
    env_set DB_TYPE sqlite
    : > "$CALLS"
    local out pass stop_line run_line
    out=$(mgr 'ASSUME_YES=true; do_postgres' 2>&1) || fail "the switch failed: $out"
    # L70: the address alone names the database
    if grep -q '^DB_TYPE=' "$NEODESK_DIR/.env"; then fail "after the switch .env still names a type: $(grep '^DB_TYPE=' "$NEODESK_DIR/.env")"; fi
    pass=$(env_get POSTGRES_PASSWORD)
    [ "${#pass}" -ge 32 ] || fail "no PostgreSQL password made"
    [ "$(env_get DATABASE_URL)" = "postgres://neodesk:${pass}@postgres:5432/neodesk?sslmode=disable" ] \
        || fail "DATABASE_URL after the switch: $(env_get DATABASE_URL)"
    [ "$(env_get COMPOSE_PROFILES)" = guacd,postgres ] || fail "COMPOSE_PROFILES after the switch: $(env_get COMPOSE_PROFILES)"
    # L446: the SQLite file where the image keeps it (DB_PATH)
    grep -qF 'compose run --rm --no-deps -T --user 10001:10001 --entrypoint sh neodesk -c exec /usr/local/bin/neodesk-server migrate -mode sqlite2pg -src "$DB_PATH"' "$CALLS" \
        || fail "the data was not moved with sqlite2pg: $(cat "$CALLS")"
    [ "$(cat "$TEST_DIR/run-env")" = "$(env_get DATABASE_URL)" ] || fail "the tool did not get the address in its environment"
    if grep -qF "$pass" "$CALLS"; then fail "the PostgreSQL password is on a command line"; fi
    if printf '%s' "$out" | grep -qF "$pass"; then fail "the PostgreSQL password was printed"; fi
    stop_line=$(grep -n 'compose stop -t 30 neodesk' "$CALLS" | head -n 1 | cut -d: -f1)
    run_line=$(grep -n 'compose run' "$CALLS" | head -n 1 | cut -d: -f1)
    { [ -n "$stop_line" ] && [ "$stop_line" -lt "$run_line" ]; } || fail "the data was moved while NeoDesk ran"
    grep -q '^run .*tar' "$CALLS" || fail "no backup before the move"
    printf '%s' "$out" | grep -q 'Migration complete: 12 rows' || fail "the tool's result is not shown: $out"
    rm -rf "$TEST_DIR"
)

# L43, decision 26: declining the move keeps the install on SQLite and
# changes nothing.
test_postgres_declined_keeps_sqlite() (
    load
    mgr 'COMMAND=install; do_install' > /dev/null 2>&1 || fail "the install failed"
    : > "$CALLS"
    local out
    out=$(mgr 'MOVE_DATA=no; do_postgres' 2>&1) || fail "declining failed: $out"
    printf '%s' "$out" | grep -q 'stays on SQLite' || fail "declining does not say the install stays on SQLite: $out"
    [ -z "$(env_get DATABASE_URL)" ] || fail "declining set DATABASE_URL"
    if grep -qE '^compose (stop|run|--profile postgres up)' "$CALLS"; then fail "declining touched the containers: $(cat "$CALLS")"; fi
    rm -rf "$TEST_DIR"
)

# L43: a move that fails leaves the install on SQLite and starts it again.
test_postgres_failed_move_keeps_sqlite() (
    load
    mgr 'COMMAND=install; do_install' > /dev/null 2>&1 || fail "the install failed"
    touch "$TEST_DIR/move-fails"
    local out
    if out=$(mgr 'ASSUME_YES=true; do_postgres' 2>&1); then fail "a failed move reported success: $out"; fi
    printf '%s' "$out" | grep -q 'stays on SQLite' || fail "the error does not say the install stays on SQLite: $out"
    [ -z "$(env_get DATABASE_URL)" ] || fail "a failed move left DATABASE_URL set"
    [ -z "$(env_get COMPOSE_PROFILES)" ] || fail "a failed move left the postgres service on"
    [ -f "$TEST_DIR/running" ] || fail "NeoDesk was not started again on SQLite"
    rm -rf "$TEST_DIR"
)

# A first install that stopped before the container ran (here: the image
# was not found) is a fresh install when run again: it starts the
# container and prints the first password.
test_failed_first_install_resumes() (
    load
    printf 'first-pass-0123
' > "$TEST_DIR/first-password"
    touch "$TEST_DIR/pull-fails"
    local out
    if out=$(mgr 'COMMAND=install; do_install' 2>&1); then fail "an install without its image reported success"; fi
    rm -f "$TEST_DIR/pull-fails"
    out=$(mgr 'COMMAND=install; do_install' 2>&1) || fail "the second try failed: $out"
    printf '%s' "$out" | grep -q 'first-pass-0123' || fail "the second try did not print the first password: $out"
    if printf '%s' "$out" | grep -q 'updating it'; then fail "the second try took the half-made install for one to update"; fi
    rm -rf "$TEST_DIR"
)

# Running the script again with the same image and the same settings
# leaves the running container alone; a changed .env makes it again.
test_rerun_without_changes_keeps_the_container() (
    load
    mgr 'COMMAND=install; do_install' > /dev/null 2>&1 || fail "the install failed"
    : > "$CALLS"
    local out
    out=$(mgr 'COMMAND=install; do_install' 2>&1) || fail "the second run failed: $out"
    printf '%s' "$out" | grep -q 'up to date' || fail "the second run does not say NeoDesk is up to date: $out"
    if grep -q 'compose stop' "$CALLS"; then fail "the second run stopped the container: $(cat "$CALLS")"; fi
    env_set TRUST_PROXY 1
    : > "$CALLS"
    out=$(mgr 'COMMAND=install; do_install' 2>&1) || fail "the run after a change of .env failed: $out"
    grep -q 'compose up -d --remove-orphans' "$CALLS" || fail "a changed .env did not reach the container"
    rm -rf "$TEST_DIR"
)

# L513: the summary's ports for a firewall in front of this machine
# follow the installers' rule: the console's port unless a reverse proxy
# is in front (TRUST_PROXY), its HTTPS port only with HTTPS_ENABLED on
test_summary_names_the_console_ports_it_serves() (
    load
    local out
    out=$(mgr 'COMMAND=install; do_install' 2>&1) || fail "the install failed: $out"
    printf '%s' "$out" | grep -q 'in front of this machine: TCP 3375, 21114-21119' || fail "a plain install's summary does not name 3375 alone: $out"
    env_set HTTPS_ENABLED true
    out=$(mgr 'COMMAND=update; do_install' 2>&1) || fail "the update with HTTPS failed: $out"
    printf '%s' "$out" | grep -q 'in front of this machine: TCP 3375, 5443, 21114-21119' || fail "the summary with HTTPS does not name 5443: $out"
    env_set TRUST_PROXY 1
    out=$(mgr 'COMMAND=update; do_install' 2>&1) || fail "the update with TRUST_PROXY failed: $out"
    printf '%s' "$out" | grep -q 'in front of this machine: TCP 21114-21119' || fail "the summary behind a reverse proxy names the console's ports: $out"
    rm -rf "$TEST_DIR"
)

# install.sh's Docker path runs neodesk-docker.sh in the compose folder
# of the install folder with the options it was given (a split install is
# taken over with --adopt), and its uninstall runs it for the same folder.
test_install_sh_runs_the_manager() (
    local dir
    dir=$(mktemp -d)
    # install_sh ARGS...: install.sh's main with ARGS; the download of the
    # manager gives a stand-in that records how it ran
    install_sh() {
        (
            INSTALL_DIR="$dir/opt"
            NEODESK_RAW_BASE="https://raw.githubusercontent.com/example/fork/feature-x"
            # shellcheck disable=SC1091
            source "$SCRIPT_ROOT/install/install.sh"
            require_root() { :; }
            fetch_url_to_file() {
                printf '#!/bin/sh\n# NEODESK_DOCKER_SOURCED stand-in\nprintf "%%s\\n" "$*" > "%s/manager-args"\nprintf "%%s\\n" "$ADMIN_PASSWORD|$NEODESK_RAW_BASE" > "%s/manager-env"\n' "$dir" "$dir" > "$2"
            }
            main
        ) > "$dir/out" 2>&1
    }
    ADMIN_PASSWORD='Docker-Admin-1' install_sh || fail "install.sh's Docker path failed: $(cat "$dir/out")"
    [ -x "$dir/opt/docker/neodesk-docker.sh" ] || fail "the manager is not in the compose folder"
    [ "$(cat "$dir/manager-args")" = "install --dir $dir/opt/docker --yes --relay-mode auto" ] \
        || fail "the manager ran as: $(cat "$dir/manager-args")"
    [ "$(cat "$dir/manager-env")" = "Docker-Admin-1|https://raw.githubusercontent.com/example/fork/feature-x" ] \
        || fail "the manager did not get the admin password and the branch's files: $(cat "$dir/manager-env")"
    # DOCKER-QUICK-12: one version in the output, the installer's
    if grep -q '1\.0\.0' "$dir/out"; then fail "install.sh names a version of its own: $(cat "$dir/out")"; fi
    mkdir -p "$dir/keys"
    install_sh --relay-servers 203.0.113.9 --version 3.6.0 --skip-docker-install --import-keys "$dir/keys" --skip-firewall \
        || fail "install.sh with options failed: $(cat "$dir/out")"
    [ "$(cat "$dir/manager-args")" = "install --dir $dir/opt/docker --yes --relay 203.0.113.9 --tag 3.6.0 --no-install-docker --import-keys $dir/keys --skip-firewall" ] \
        || fail "the manager ran with the options as: $(cat "$dir/manager-args")"
    printf 'NEODESK_DOCKER_LAYOUT=split\n' > "$dir/opt/docker/.env"
    install_sh || fail "install.sh on a split install failed: $(cat "$dir/out")"
    case "$(cat "$dir/manager-args")" in *" --adopt"*) ;; *) fail "a split install is not taken over: $(cat "$dir/manager-args")" ;; esac
    install_sh --uninstall --purge || fail "install.sh's uninstall failed: $(cat "$dir/out")"
    [ "$(cat "$dir/manager-args")" = "uninstall --dir $dir/opt/docker --purge --yes" ] \
        || fail "the uninstall ran the manager as: $(cat "$dir/manager-args")"
    rm -rf "$dir"
)

# --domain (with --email): Traefik in front of the console with Let's
# Encrypt. .env gets the profile, the domain, the contact, the console's
# port on this host only and the proxy trust; the summary names the HTTPS
# address and ports 80 and 443. --no-traefik takes all of it back and
# removes the Traefik container.
test_traefik_with_letsencrypt() (
    load
    local out
    out=$(mgr 'getent() { return 2; }
               DOMAIN_SET=console.example.com; LETSENCRYPT_EMAIL_SET=ops@neodesk-test.de; TRAEFIK_ACTION=on
               COMMAND=install; do_install' 2>&1) || fail "an install with --domain failed: $out"
    local line
    for line in COMPOSE_PROFILES=traefik NEODESK_DOMAIN=console.example.com LETSENCRYPT_EMAIL=ops@neodesk-test.de \
        NEODESK_CONSOLE_BIND=127.0.0.1 TRUST_PROXY=1 PANEL_PUBLIC_URL=https://console.example.com; do
        grep -qx "$line" "$NEODESK_DIR/.env" || fail ".env has no $line: $(cat "$NEODESK_DIR/.env")"
    done
    if grep -q '^LETSENCRYPT_CA_SERVER=' "$NEODESK_DIR/.env"; then fail "a production install uses another CA server"; fi
    printf '%s' "$out" | grep -q 'Web console: *https://console.example.com' || fail "no HTTPS console address in: $out"
    printf '%s' "$out" | grep -q 'TCP 80, 443, 21114' || fail "the firewall line does not name 80 and 443: $out"
    printf '%s' "$out" | grep -q 'has its certificate' || fail "the certificate was not checked: $out"
    printf '%s' "$out" | grep -q 'does not resolve' || fail "a name that does not resolve gives no warning: $out"

    out=$(mgr 'DOMAIN_SET=console.example.com; LETSENCRYPT_STAGING=true; TRAEFIK_ACTION=on; getent() { return 2; }
               COMMAND=update; do_install' 2>&1) || fail "the switch to the staging server failed: $out"
    grep -qx 'LETSENCRYPT_CA_SERVER=https://acme-staging-v02.api.letsencrypt.org/directory' "$NEODESK_DIR/.env" \
        || fail ".env has not the staging server: $(cat "$NEODESK_DIR/.env")"
    grep -qx 'LETSENCRYPT_EMAIL=ops@neodesk-test.de' "$NEODESK_DIR/.env" || fail "the contact of the first run was lost"

    : > "$CALLS"
    out=$(mgr 'TRAEFIK_ACTION=off; COMMAND=update; do_install' 2>&1) || fail "--no-traefik failed: $out"
    grep -q 'rm -sf traefik' "$CALLS" || fail "--no-traefik left the Traefik container: $(cat "$CALLS")"
    for line in COMPOSE_PROFILES NEODESK_DOMAIN NEODESK_CONSOLE_BIND TRUST_PROXY PANEL_PUBLIC_URL LETSENCRYPT_CA_SERVER; do
        if grep -q "^$line=" "$NEODESK_DIR/.env"; then fail "--no-traefik left $line: $(cat "$NEODESK_DIR/.env")"; fi
    done
    printf '%s' "$out" | grep -q 'Web console: *http://203.0.113.10:3375' || fail "no plain console address after --no-traefik: $out"
    rm -rf "$TEST_DIR"
)

# --domain needs a DNS name; ports 80 and 443 must be free for Traefik,
# and the error says what to do. HTTPS-11: --email is optional, but an
# address Let's Encrypt refuses (example.com, the installer's old
# example) stops the run before anything changes.
test_traefik_errors_say_what_to_do() (
    load
    local out
    out=$(mgr 'parse_args --domain console.example.com; printf "%s\n" "$TRAEFIK_ACTION"' 2>&1) || fail "--domain without --email was refused: $out"
    [ "$out" = on ] || fail "--domain without --email did not turn Traefik on: $out"
    if out=$(mgr 'parse_args --domain console.example.com --email admin@example.com' 2>&1); then fail "an address at example.com went on"; fi
    printf '%s' "$out" | grep -q 'refuses addresses at example.com' || fail "the error does not say why: $out"
    if out=$(mgr 'parse_args --domain console.example.com --email me@mail.example.org' 2>&1); then fail "an address below example.org went on"; fi
    if out=$(mgr 'parse_args --domain 203.0.113.10 --email ops@neodesk-test.de' 2>&1); then fail "an IP address passed as --domain"; fi
    printf '%s' "$out" | grep -q 'DNS name' || fail "the --domain error does not say what it needs: $out"
    if out=$(mgr 'parse_args --domain console.example.com --email nobody' 2>&1); then fail "a bad e-mail address passed"; fi
    if out=$(mgr 'parse_args --domain console.example.com --email ops@neodesk-test.de --no-traefik' 2>&1); then
        fail "--domain with --no-traefik went on"
    fi
    out=$(mgr 'parse_args --domain Console.Example.com --email ops@neodesk-test.de; printf "%s %s\n" "$DOMAIN_SET" "$TRAEFIK_ACTION"' 2>&1) \
        || fail "a valid --domain was refused: $out"
    [ "$out" = "console.example.com on" ] || fail "--domain was read as: $out"

    printf 'apache2\n' > "$TEST_DIR/taken-80"
    if out=$(mgr 'DOMAIN_SET=console.example.com; LETSENCRYPT_EMAIL_SET=ops@neodesk-test.de; TRAEFIK_ACTION=on
                  COMMAND=install; do_install' 2>&1); then fail "an install with port 80 taken went on"; fi
    printf '%s' "$out" | grep -q 'TCP 80: apache2' || fail "the port error does not name the program: $out"
    printf '%s' "$out" | grep -q 'Traefik needs ports 80 and 443' || fail "the port error does not say why: $out"
    rm -f "$TEST_DIR/taken-80"

    mgr 'COMMAND=install; do_install' > /dev/null 2>&1 || fail "the plain install failed"
    printf 'nginx\n' > "$TEST_DIR/taken-443"
    if out=$(mgr 'DOMAIN_SET=console.example.com; LETSENCRYPT_EMAIL_SET=ops@neodesk-test.de; TRAEFIK_ACTION=on
                  COMMAND=update; do_install' 2>&1); then fail "turning Traefik on with port 443 taken went on"; fi
    printf '%s' "$out" | grep -q 'TCP 443: nginx' || fail "the update's port error does not name the program: $out"
    if grep -q '^NEODESK_DOMAIN=' "$NEODESK_DIR/.env"; then fail "the refused switch changed .env"; fi
    rm -rf "$TEST_DIR"
)

# install.sh passes --domain, --letsencrypt-email and
# --letsencrypt-staging to the manager; HTTPS-16: it takes the manager's
# spellings --email and --no-traefik as well.
test_install_sh_passes_the_domain() (
    local dir
    dir=$(mktemp -d)
    # install_sh ARGS...: install.sh's main with ARGS and a stand-in manager
    install_sh() {
        (
            INSTALL_DIR="$dir/opt"
            # shellcheck disable=SC1091
            source "$SCRIPT_ROOT/install/install.sh" "$@"
            require_root() { :; }
            fetch_manager() {
                mkdir -p "$(dirname "$2")"
                printf '#!/bin/sh\n# NEODESK_DOCKER_SOURCED stand-in\nprintf "%%s\\n" "$*" > "%s/manager-args"\n' "$dir" > "$2"
                chmod +x "$2"
            }
            main
        ) > "$dir/out" 2>&1
    }
    install_sh --domain console.example.com --letsencrypt-email ops@neodesk-test.de --letsencrypt-staging || fail "install.sh with --domain failed: $(cat "$dir/out")"
    [ "$(cat "$dir/manager-args")" = "install --dir $dir/opt/docker --yes --relay-mode auto --domain console.example.com --email ops@neodesk-test.de --letsencrypt-staging" ] \
        || fail "the manager ran with the domain as: $(cat "$dir/manager-args")"
    install_sh --domain console.example.com --email ops@neodesk-test.de || fail "install.sh with --email failed: $(cat "$dir/out")"
    [ "$(cat "$dir/manager-args")" = "install --dir $dir/opt/docker --yes --relay-mode auto --domain console.example.com --email ops@neodesk-test.de" ] \
        || fail "the manager ran with --email as: $(cat "$dir/manager-args")"
    install_sh --no-traefik || fail "install.sh with --no-traefik failed: $(cat "$dir/out")"
    [ "$(cat "$dir/manager-args")" = "install --dir $dir/opt/docker --yes --relay-mode auto --no-traefik" ] \
        || fail "the manager ran with --no-traefik as: $(cat "$dir/manager-args")"
    install_sh --no-ipv6 || fail "install.sh with --no-ipv6 failed: $(cat "$dir/out")"
    [ "$(cat "$dir/manager-args")" = "install --dir $dir/opt/docker --yes --relay-mode auto --no-ipv6" ] \
        || fail "the manager ran with --no-ipv6 as: $(cat "$dir/manager-args")"
    rm -rf "$dir"
)

# traefik_docker ARGS...: the Docker stand-in, with the volume of
# Traefik's certificates in $TEST_DIR/le: a script run on it (-c) runs
# here on that folder, and compose logs prints $TEST_DIR/traefik-log
traefik_docker() {
    if [ "$1" = volume ] && [ "$2" = inspect ] && [ "${3:-}" = neodesk-traefik-letsencrypt ]; then
        printf '%s\n' "$*" >> "$CALLS"
        [ -d "$TEST_DIR/le" ]
        return
    fi
    if [ "$1" = run ] && [[ " $* " == *" neodesk-traefik-letsencrypt:/le"* ]]; then
        printf '%s\n' "$*" >> "$CALLS"
        local arg script="" prev="" rest=()
        for arg in "$@"; do
            if [ "$prev" = -c ]; then script="$arg"; prev=""; rest=(); continue; fi
            [ -z "$script" ] || rest+=("$arg")
            prev="$arg"
        done
        sh -c "${script//\/le/$TEST_DIR/le}" "${rest[@]}"
        return
    fi
    if [ "$1" = compose ] && [ "$2" = logs ]; then
        cat "$TEST_DIR/traefik-log" 2>/dev/null || true
        return 0
    fi
    command_docker "$@"
}

# HTTPS-1: with the staging server the certificate check wants one of its
# certificates; Traefik's own default certificate is no success. HTTPS-4:
# a missing certificate is told with Traefik's own cause and how to try
# again.
test_staging_needs_a_staging_certificate() (
    load
    local out
    printf '%s\n' '2026-11-27T00:00:00Z ERR Unable to obtain ACME certificate for domains error="unable to generate a certificate for the domains [console.example.com]: acme: error: 400 :: urn:ietf:params:acme:error:invalidContact :: Error validating contact(s) :: contact email has forbidden domain \"example.com\"" ACME CA=https://acme-staging-v02.api.letsencrypt.org/directory' > "$TEST_DIR/traefik-log"
    # With the colours Traefik writes into its log without --log.nocolor
    sed -i 's/ ERR / \x1b[31mERR\x1b[0m /; s/error="/\x1b[36merror=\x1b[0m\x1b[31m\x1b[1m"/' "$TEST_DIR/traefik-log"
    out=$(mgr "$(declare -f traefik_docker)"'
              docker() { traefik_docker "$@"; }
              openssl() { [ "$1" != s_client ] || { cat > /dev/null; return 0; }; cat > /dev/null; printf "issuer=CN = TRAEFIK DEFAULT CERT\n"; }
              getent() { return 2; }
              DOMAIN_SET=console.example.com; LETSENCRYPT_STAGING=true; TRAEFIK_ACTION=on
              COMMAND=install; do_install' 2>&1) || fail "the staging install failed: $out"
    if printf '%s' "$out" | grep -q 'has a certificate'; then fail "Traefik's default certificate passed as a staging one: $out"; fi
    printf '%s' "$out" | grep -q 'no certificate from Let.s Encrypt yet' || fail "no warning without a certificate: $out"
    printf '%s' "$out" | grep -qF 'contact email has forbidden domain "example.com"' || fail "the warning does not give Traefik's cause: $out"
    printf '%s' "$out" | grep -q 'run this script again' || fail "the warning does not say how to try again: $out"
    out=$(mgr "$(declare -f traefik_docker)"'
              docker() { traefik_docker "$@"; }
              openssl() { cat > /dev/null; [ "$1" = s_client ] || printf "issuer=C = US, O = (STAGING) Let'"'"'s Encrypt, CN = (STAGING) Ersatz Emmer YR2\n"; }
              getent() { return 2; }
              DOMAIN_SET=console.example.com; LETSENCRYPT_STAGING=true; TRAEFIK_ACTION=on
              COMMAND=update; do_install' 2>&1) || fail "the staging run failed: $out"
    printf '%s' "$out" | grep -q "certificate of Let's Encrypt's staging server" || fail "a staging certificate was not taken: $out"
    rm -rf "$TEST_DIR"
)

# HTTPS-2: going from the staging server to the production one, the
# staging account and certificate in acme.json move aside, with Traefik
# stopped, and Traefik is made again, so it asks the production server.
# An acme.json of the server in use stays.
test_staging_to_production_renews() (
    load
    local out
    mkdir -p "$TEST_DIR/le"
    mgr "$(declare -f traefik_docker)"'
         docker() { traefik_docker "$@"; }
         getent() { return 2; }
         DOMAIN_SET=console.example.com; LETSENCRYPT_STAGING=true; TRAEFIK_ACTION=on
         COMMAND=install; do_install' > "$TEST_DIR/staging.out" 2>&1 || fail "the staging install failed: $(cat "$TEST_DIR/staging.out")"
    printf '{"letsencrypt":{"Account":{"Registration":{"uri":"https://acme-staging-v02.api.letsencrypt.org/acme/acct/1"}}}}\n' > "$TEST_DIR/le/acme.json"
    : > "$CALLS"
    out=$(mgr "$(declare -f traefik_docker)"'
         docker() { traefik_docker "$@"; }
              getent() { return 2; }
              DOMAIN_SET=console.example.com; TRAEFIK_ACTION=on
              COMMAND=update; do_install' 2>&1) || fail "the switch to production failed: $out"
    [ ! -e "$TEST_DIR/le/acme.json" ] || fail "the staging acme.json stayed: $(cat "$TEST_DIR/le/acme.json")"
    ls "$TEST_DIR/le"/acme.json.other-server-* > /dev/null 2>&1 || fail "the staging acme.json was not kept aside: $(ls "$TEST_DIR/le")"
    local stop move recreate
    stop=$(grep -n '^compose --profile traefik stop traefik' "$CALLS" | head -n 1 | cut -d: -f1)
    move=$(grep -n 'mv /le/acme.json' "$CALLS" | head -n 1 | cut -d: -f1)
    recreate=$(grep -n '^compose up -d --no-deps --force-recreate traefik' "$CALLS" | head -n 1 | cut -d: -f1)
    { [ -n "$stop" ] && [ -n "$move" ] && [ -n "$recreate" ] && [ "$stop" -lt "$move" ] && [ "$move" -lt "$recreate" ]; } \
        || fail "Traefik was not stopped, its storage moved and Traefik made again, in that order: $(cat "$CALLS")"
    printf '%s' "$out" | grep -q 'has its certificate' || fail "the production certificate was not checked: $out"

    printf '{"letsencrypt":{"Account":{"Registration":{"uri":"https://acme-v02.api.letsencrypt.org/acme/acct/2"}}}}\n' > "$TEST_DIR/le/acme.json"
    mgr "$(declare -f traefik_docker)"'
         docker() { traefik_docker "$@"; }
         COMMAND=update; do_install' > /dev/null 2>&1 || fail "the update on production failed"
    [ -e "$TEST_DIR/le/acme.json" ] || fail "an acme.json of the production server was moved"
    rm -rf "$TEST_DIR"
)

# HTTPS-3, HTTPS-17: --domain on an install that runs Traefik, with the
# same image: Traefik is made again (it reads its routing only when it is
# made, and asks Let's Encrypt again), and nothing is stopped or backed up.
test_domain_change_remakes_traefik() (
    load
    local out
    mgr 'getent() { return 2; }
         DOMAIN_SET=console.example.com; TRAEFIK_ACTION=on
         COMMAND=install; do_install' > /dev/null 2>&1 || fail "the install with --domain failed"
    : > "$CALLS"
    out=$(mgr 'getent() { return 2; }
               DOMAIN_SET=remote.example.net; TRAEFIK_ACTION=on
               COMMAND=update; do_install' 2>&1) || fail "the domain change failed: $out"
    grep -qx 'NEODESK_DOMAIN=remote.example.net' "$NEODESK_DIR/.env" || fail ".env has not the new domain: $(cat "$NEODESK_DIR/.env")"
    grep -q '^compose up -d --no-deps --force-recreate traefik' "$CALLS" || fail "Traefik was not made again: $(cat "$CALLS")"
    if grep -qE '^compose stop|^run .*czf' "$CALLS"; then fail "a change of the domain stopped NeoDesk or backed it up: $(cat "$CALLS")"; fi
    printf '%s' "$out" | grep -q 'Web console: *https://remote.example.net' || fail "the summary does not name the new domain: $out"
    # The same --domain again: Traefik is made again, so it tries once more
    : > "$CALLS"
    mgr 'getent() { return 2; }
         DOMAIN_SET=remote.example.net; TRAEFIK_ACTION=on
         COMMAND=update; do_install' > /dev/null 2>&1 || fail "the second run with the domain failed"
    grep -q '^compose up -d --no-deps --force-recreate traefik' "$CALLS" || fail "a second --domain did not make Traefik try again: $(cat "$CALLS")"
    rm -rf "$TEST_DIR"
)

# HTTPS-6, owner call docker-ipv6: a new install on Docker Engine 27 or
# newer gets the IPv6 network and its summary says Docker publishes the
# ports past ufw on IPv6 too. An older engine, a host where Docker can
# make no IPv6 network and --no-ipv6 keep IPv4, the first two saying why.
# Installs from before (no NEODESK_IPV6 in .env, also one whose container
# was removed) stay as they are.
test_new_install_gets_ipv6() (
    load
    local out
    out=$(mgr 'COMMAND=install; do_install' 2>&1) || fail "a fresh install failed: $out"
    grep -qx 'NEODESK_IPV6=true' "$NEODESK_DIR/.env" || fail "a new install on Engine 29 has no IPv6: $(cat "$NEODESK_DIR/.env")"
    grep -qx 'network create --ipv6 neodesk-ipv6-check' "$CALLS" || fail "IPv6 was not checked with a network: $(cat "$CALLS")"
    grep -qx 'network rm neodesk-ipv6-check' "$CALLS" || fail "the check's network was not removed: $(cat "$CALLS")"
    printf '%s' "$out" | grep -q 'publishes the ports past ufw and firewalld on IPv6' || fail "the summary does not say IPv6 ports pass ufw: $out"
    # A rerun keeps the setting, also when the engine says otherwise now
    printf '26.1.4\n' > "$TEST_DIR/engine"
    out=$(mgr 'COMMAND=install; do_install' 2>&1) || fail "the second run failed: $out"
    grep -qx 'NEODESK_IPV6=true' "$NEODESK_DIR/.env" || fail "a rerun changed the IPv6 setting: $(cat "$NEODESK_DIR/.env")"
    rm -rf "$TEST_DIR"

    load
    printf '26.1.4\n' > "$TEST_DIR/engine"
    out=$(mgr 'COMMAND=install; do_install' 2>&1) || fail "an install on Engine 26 failed: $out"
    grep -qx 'NEODESK_IPV6=false' "$NEODESK_DIR/.env" || fail "an install on Engine 26 is not on IPv4: $(cat "$NEODESK_DIR/.env")"
    printf '%s' "$out" | grep -q 'IPv6 needs Docker Engine 27 or newer, this is 26.1.4' || fail "the install on Engine 26 does not say why: $out"
    if grep -q 'network create' "$CALLS"; then fail "an install on Engine 26 tried an IPv6 network"; fi
    if printf '%s' "$out" | grep -q 'on IPv6 as on IPv4'; then fail "an IPv4 install's summary talks about IPv6 ports: $out"; fi
    rm -rf "$TEST_DIR"

    load
    : > "$TEST_DIR/no-ipv6"
    out=$(mgr 'COMMAND=install; do_install' 2>&1) || fail "an install without IPv6 on the host failed: $out"
    grep -qx 'NEODESK_IPV6=false' "$NEODESK_DIR/.env" || fail "a host without IPv6 networks is not on IPv4: $(cat "$NEODESK_DIR/.env")"
    printf '%s' "$out" | grep -q 'could not make an IPv6 network here (Error response from daemon: could not find' \
        || fail "the install without IPv6 does not say why: $out"
    rm -rf "$TEST_DIR"

    load
    out=$(mgr 'parse_args --no-ipv6; do_install' 2>&1) || fail "an install with --no-ipv6 failed: $out"
    grep -qx 'NEODESK_IPV6=false' "$NEODESK_DIR/.env" || fail "--no-ipv6 did not keep IPv4: $(cat "$NEODESK_DIR/.env")"
    if grep -q 'network create' "$CALLS"; then fail "--no-ipv6 still tried an IPv6 network"; fi
    rm -rf "$TEST_DIR"

    # An install from before: one whose container is gone, then running
    load
    mkdir -p "$NEODESK_DIR"
    printf 'NEODESK_DOCKER_LAYOUT=single\nRELAY_SERVERS=203.0.113.10\n' > "$NEODESK_DIR/.env"
    out=$(mgr 'COMMAND=install; do_install' 2>&1) || fail "the install with an earlier .env failed: $out"
    if grep -q '^NEODESK_IPV6=' "$NEODESK_DIR/.env"; then fail "an earlier install whose container was gone got an IPv6 setting"; fi
    out=$(mgr 'COMMAND=update; do_install' 2>&1) || fail "the update of an earlier install failed: $out"
    if grep -q '^NEODESK_IPV6=' "$NEODESK_DIR/.env"; then fail "an update gave an earlier install an IPv6 setting"; fi
    rm -rf "$TEST_DIR"
)

# HTTPS-10: the DNS check knows this server's IPv6 addresses: a name with
# only an AAAA record of the server gives no warning, another one does.
test_dns_check_knows_ipv6() (
    load
    local out
    out=$(mgr 'getent() { printf "2001:db8:5::2e STREAM name\n"; }
               ip() { [ "$1" = -6 ] && printf "    inet6 2001:db8:5::2e/128 scope global\n"; }
               RELAY_SERVERS_SET=203.0.113.10; check_domain_dns console.example.com' 2>&1) || fail "the DNS check failed: $out"
    [ -z "$out" ] || fail "a name with the server's IPv6 address gave a warning: $out"
    out=$(mgr 'getent() { printf "2001:db8:9::1 STREAM name\n"; }
               ip() { [ "$1" = -6 ] && printf "    inet6 2001:db8:5::2e/128 scope global\n"; }
               RELAY_SERVERS_SET=203.0.113.10; check_domain_dns console.example.com' 2>&1)
    printf '%s' "$out" | grep -q 'not at this server' || fail "a name elsewhere gave no warning: $out"
    rm -rf "$TEST_DIR"
)


# The compose file and the manager's own copy come from the release the
# image tag names, checked against the release's SHA256SUMS: v<version>
# for a version, the newest release for latest (a pre-release too, not the
# rolling dev one), the dev release for dev. A release from before the
# files were attached (3.6.0-rc.4 and older) has the compose file at its
# tag, at the root. Nothing comes from a branch's raw files, and a file
# that does not match stops the run before anything changes.
test_files_come_from_the_release() (
    local dir rel
    dir=$(mktemp -d)
    rel="$dir/release"
    mkdir -p "$rel" "$dir/install"
    # The install's copy of the manager, without a compose file next to it
    cp "$SCRIPT_ROOT/install/neodesk-docker.sh" "$dir/install/neodesk-docker.sh"
    { cat "$SCRIPT_ROOT/install/docker-compose.quick.single.yml"; printf "# the release's copy\n"; } > "$rel/docker-compose.quick.single.yml"
    { cat "$SCRIPT_ROOT/install/neodesk-docker.sh"; printf "# the release's copy\n"; } > "$rel/neodesk-docker.sh"
    (cd "$rel" && sha256sum docker-compose.quick.single.yml neodesk-docker.sh > SHA256SUMS)
    cat > "$dir/run.sh" <<'RUN'
NEODESK_DOCKER_SOURCED=1
source "$DIR/install/neodesk-docker.sh"
NEODESK_DIR="$DIR/install"
curl() {
    local a prev="" dest="" url=""
    for a in "$@"; do [ "$prev" = -o ] && dest="$a"; prev="$a"; url="$a"; done
    printf '%s\n' "$url" >> "$DIR/fetched"
    case "$url" in
        https://api.github.com/*)
            printf '[{"tag_name": "dev", "draft": false}, {"tag_name": "v3.6.0-rc.6", "draft": true}, {"tag_name": "v3.6.0-rc.5", "draft": false}]' ;;
        https://raw.githubusercontent.com/neodeskapp/neodesk/v3.6.0-rc.4/docker-compose.quick.single.yml)
            cp "$RELEASE/docker-compose.quick.single.yml" "$dest" ;;
        https://github.com/neodeskapp/neodesk/releases/download/v3.6.0-rc.5/*|https://github.com/neodeskapp/neodesk/releases/download/dev/*)
            [ -f "$RELEASE/${url##*/}" ] || return 22
            cp "$RELEASE/${url##*/}" "$dest" ;;
        *) return 22 ;;
    esac
}
eval "$CODE"
RUN
    # run TAG CODE: CODE in the manager, with the image tag TAG
    run() { : > "$dir/fetched"; DIR="$dir" RELEASE="$rel" NEODESK_IMAGE_TAG="$1" CODE="$2" bash "$dir/run.sh" > "$dir/out" 2>&1; }
    # from TAG URL: the compose file of the image tag TAG came from URL
    from() {
        rm -f "$dir/compose.yml"
        run "$1" 'fetch_compose_file "$DIR/compose.yml"' || fail "the compose file of $1: $(cat "$dir/out")"
        cmp -s "$dir/compose.yml" "$rel/docker-compose.quick.single.yml" || fail "the compose file of $1 is not the release's"
        grep -qxF "$2" "$dir/fetched" || fail "the compose file of $1 did not come from $2: $(tr '\n' ' ' < "$dir/fetched")"
        if grep -q 'githubusercontent.com/neodeskapp/neodesk/main' "$dir/fetched"; then fail "a raw file of main was fetched for $1"; fi
    }
    local releases=https://github.com/neodeskapp/neodesk/releases/download
    from 3.6.0-rc.5 "$releases/v3.6.0-rc.5/docker-compose.quick.single.yml"
    grep -qxF "$releases/v3.6.0-rc.5/SHA256SUMS" "$dir/fetched" || fail "the compose file was not checked against the release's SHA256SUMS"
    from latest "$releases/v3.6.0-rc.5/docker-compose.quick.single.yml"
    from dev "$releases/dev/docker-compose.quick.single.yml"
    from 3.6.0-rc.4 https://raw.githubusercontent.com/neodeskapp/neodesk/v3.6.0-rc.4/docker-compose.quick.single.yml
    if run local 'fetch_compose_file "$DIR/compose.yml"'; then fail "a local image found a release's compose file"; fi

    # The manager's own copy follows the release
    run 3.6.0-rc.5 update_manager_copy || fail "the manager's update failed: $(cat "$dir/out")"
    cmp -s "$dir/install/neodesk-docker.sh" "$rel/neodesk-docker.sh" || fail "the install's manager is not the release's after the update"
    cp "$SCRIPT_ROOT/install/neodesk-docker.sh" "$dir/install/neodesk-docker.sh"
    run 3.6.0-rc.4 update_manager_copy || fail "the manager's update failed: $(cat "$dir/out")"
    cmp -s "$dir/install/neodesk-docker.sh" "$SCRIPT_ROOT/install/neodesk-docker.sh" || fail "a release without the manager replaced it"

    # Files that do not match the release's SHA256SUMS
    printf '# changed\n' >> "$rel/docker-compose.quick.single.yml"
    printf '# changed\n' >> "$rel/neodesk-docker.sh"
    rm -f "$dir/compose.yml"
    if run 3.6.0-rc.5 'fetch_compose_file "$DIR/compose.yml"'; then fail "a changed compose file was taken"; fi
    grep -q "does not match" "$dir/out" || fail "a changed compose file says: $(cat "$dir/out")"
    [ ! -e "$dir/compose.yml" ] || fail "a changed compose file was left in place"
    run 3.6.0-rc.5 update_manager_copy
    cmp -s "$dir/install/neodesk-docker.sh" "$SCRIPT_ROOT/install/neodesk-docker.sh" || fail "a changed manager replaced the install's copy"
    rm -rf "$dir"
)

# install.sh takes the managers from the release of its own version (or
# of --version), checked against the release's SHA256SUMS: the install.sh
# neode.sk serves is the newest release's, so it takes that release's.
# While its own release is not out (a checkout ahead of the releases) it
# takes the newest release's. A version given that has no release, or a
# file that does not match, stops it before anything is installed. The
# test sets install.sh's own version (OWN), so the checkout's version, a
# release's or a development one, does not decide the outcome.
test_install_sh_takes_the_release_files() (
    local dir rel
    dir=$(mktemp -d)
    rel="$dir/release"
    mkdir -p "$rel"
    { printf '#!/bin/sh\n# NEODESK_DOCKER_SOURCED stand-in\n'; head -c 5000 /dev/zero | tr '\0' '#'; printf '\n'; } > "$rel/neodesk-docker.sh"
    (cd "$rel" && sha256sum neodesk-docker.sh > SHA256SUMS)
    # fetch ARGS...: install.sh with ARGS, of the version OWN when set,
    # fetches the Docker manager
    fetch() {
        rm -f "$dir/manager.sh"
        : > "$dir/fetched"
        (
            INSTALL_DIR="$dir/opt"
            unset NEODESK_RAW_BASE NEODESK_BRANCH RELEASE_BASE_URL NEODESK_VERSION
            # shellcheck disable=SC1091
            source "$SCRIPT_ROOT/install/install.sh" "$@"
            [ -z "${OWN:-}" ] || NEODESK_VERSION="$OWN"
            # As piped from curl: no files next to it
            INSTALLER_FOLDER=""
            curl() {
                local a prev="" dest="" url=""
                for a in "$@"; do [ "$prev" = -o ] && dest="$a"; prev="$a"; url="$a"; done
                printf '%s\n' "$url" >> "$dir/fetched"
                case "$url" in
                    https://api.github.com/*) printf '[{"tag_name": "dev", "draft": false}, {"tag_name": "v3.6.0-rc.5", "draft": false}]' ;;
                    https://github.com/neodeskapp/neodesk/releases/download/v3.6.0-rc.5/*)
                        [ -f "$rel/${url##*/}" ] || return 22
                        cp "$rel/${url##*/}" "$dest" ;;
                    *) return 22 ;;
                esac
            }
            fetch_docker_manager "$dir/manager.sh"
        ) > "$dir/out" 2>&1
    }
    fetch --version 3.6.0-rc.5 || fail "the manager of --version 3.6.0-rc.5: $(cat "$dir/out")"
    cmp -s "$dir/manager.sh" "$rel/neodesk-docker.sh" || fail "the manager is not the release's"
    grep -qx https://github.com/neodeskapp/neodesk/releases/download/v3.6.0-rc.5/SHA256SUMS "$dir/fetched" \
        || fail "the manager was not checked against the release's SHA256SUMS: $(tr '\n' ' ' < "$dir/fetched")"
    if grep -q githubusercontent "$dir/fetched"; then fail "a raw file was fetched: $(tr '\n' ' ' < "$dir/fetched")"; fi
    OWN=3.6.0-rc.5 fetch || fail "the manager of install.sh's own release: $(cat "$dir/out")"
    cmp -s "$dir/manager.sh" "$rel/neodesk-docker.sh" || fail "install.sh of a release did not take its release's manager"
    if grep -q 'taking the files of the newest release' "$dir/out"; then fail "install.sh of a release looked for another one: $(cat "$dir/out")"; fi
    OWN=9.9.9 fetch || fail "the manager of a checkout ahead of the releases: $(cat "$dir/out")"
    cmp -s "$dir/manager.sh" "$rel/neodesk-docker.sh" || fail "without its own release out, install.sh did not take the newest release's manager"
    grep -q 'There is no release 9.9.9 yet: taking the files of the newest release, v3.6.0-rc.5' "$dir/out" \
        || fail "install.sh does not say it takes the newest release: $(cat "$dir/out")"
    if fetch --version 3.6.0-rc.9; then fail "a version without a release installed a manager"; fi
    [ ! -e "$dir/manager.sh" ] || fail "a version without a release left a manager"
    printf '# changed\n' >> "$rel/neodesk-docker.sh"
    if fetch --version 3.6.0-rc.5; then fail "a manager that does not match the release's SHA256SUMS was taken"; fi
    grep -q 'does not match its SHA256SUMS' "$dir/out" || fail "a changed manager says: $(cat "$dir/out")"
    [ ! -e "$dir/manager.sh" ] || fail "a changed manager was left in place"
    rm -rf "$dir"
)

# A250: the console's port is 3375. An install whose compose file
# published the old default, 5000, and whose .env names no host port
# moves to 3375 at its update, and the update says so with how to keep
# 5000; one whose .env names a port keeps it, and nothing is said.
test_update_moves_the_default_console_port() (
    load
    local out
    out=$(mgr 'COMMAND=install; do_install' 2>&1) || fail "the install failed: $out"
    # The compose file of 3.6.0-rc.4, which published 5000
    sed -e 's/NEODESK_CONSOLE_PORT:-3375}:3375/NEODESK_CONSOLE_PORT:-5000}:5000/' "$NEODESK_DIR/docker-compose.yml" > "$TEST_DIR/old.yml"
    grep -q 'NEODESK_CONSOLE_PORT:-5000}:5000' "$TEST_DIR/old.yml" || fail "the test's old compose file publishes no 5000"
    cp "$TEST_DIR/old.yml" "$NEODESK_DIR/docker-compose.yml"
    sha256sum "$NEODESK_DIR/docker-compose.yml" | cut -d' ' -f1 > "$NEODESK_DIR/.compose.sha256"
    printf 'sha256:new\n' > "$TEST_DIR/pulled"
    printf 'sha256:new\n' > "$TEST_DIR/good-image"
    out=$(mgr 'COMMAND=update; do_install' 2>&1) || fail "the update failed: $out"
    printf '%s' "$out" | grep -q 'The console now answers on port 3375; it was 5000' || fail "the update does not say the console moved: $out"
    printf '%s' "$out" | grep -q "NEODESK_CONSOLE_PORT=5000 in $NEODESK_DIR/.env" || fail "the update does not say how to keep 5000: $out"
    grep -q 'NEODESK_CONSOLE_PORT:-3375}:3375' "$NEODESK_DIR/docker-compose.yml" || fail "the update kept the old compose file"
    # A host port in .env stays, and nothing is said
    cp "$TEST_DIR/old.yml" "$NEODESK_DIR/docker-compose.yml"
    sha256sum "$NEODESK_DIR/docker-compose.yml" | cut -d' ' -f1 > "$NEODESK_DIR/.compose.sha256"
    printf 'NEODESK_CONSOLE_PORT=5000\n' >> "$NEODESK_DIR/.env"
    printf 'sha256:newer\n' > "$TEST_DIR/pulled"
    printf 'sha256:newer\n' > "$TEST_DIR/good-image"
    out=$(mgr 'COMMAND=update; do_install' 2>&1) || fail "the update with a port of its own failed: $out"
    if printf '%s' "$out" | grep -q 'The console now answers on port'; then fail "an install with a port of its own was told it moved: $out"; fi
    printf '%s' "$out" | grep -q 'Web console: *http://203.0.113.10:5000' || fail "the port of .env does not win: $out"
    rm -rf "$TEST_DIR"
)

# DOCKER-QUICK-5: install.sh --help lists every option, also piped into
# bash (curl ... | sudo bash -s -- --help), where it has no file to read.
test_install_sh_help_when_piped() (
    local help opt
    help=$(bash -s -- --help < "$SCRIPT_ROOT/install/install.sh" 2>&1) || fail "install.sh --help through a pipe failed: $help"
    for opt in $(sed -n '/^while \[ \$# -gt 0 \]; do/,/^done/p' "$SCRIPT_ROOT/install/install.sh" \
        | grep -oE '^ +--[a-z-]+' | tr -d ' ' | grep -vx -- '--split' | sort -u); do
        printf '%s' "$help" | grep -q -- "$opt" || fail "install.sh --help does not list $opt: $help"
    done
)

# fw_rules: the rules of the stand-in firewall, sorted, on one line
fw_rules() {
    if [ "$(cat "$TEST_DIR/fw")" = ufw ]; then
        awk '$2 == "ALLOW" { print $1 }' "$TEST_DIR/fw-rules" 2>/dev/null | sort -u | tr '\n' ' '
    else
        sort -u "$TEST_DIR/fw-rules" 2>/dev/null | tr '\n' ' '
    fi
}

# DOCKER-QUICK-7, HTTPS-9: the manager opens what NeoDesk serves in ufw
# or firewalld (install.sh no longer does it itself), before the
# container starts: the client ports, the console's 3375, its 5443 with
# HTTPS_ENABLED; the console's ports close again behind a reverse proxy
# (TRUST_PROXY), and uninstall closes everything it opened. A second run
# opens nothing twice.
test_firewall_follows_what_it_serves() (
    local fw out clients first up
    clients="21114/tcp 21115/tcp 21116/tcp 21116/udp 21117/tcp 21118/tcp 21119/tcp 21121/tcp 21122/tcp 21123/tcp"
    sorted() { printf '%s\n' "$@" | sort -u | tr '\n' ' '; }
    for fw in ufw firewalld; do
        load
        printf '%s\n' "$fw" > "$TEST_DIR/fw"
        out=$(mgr 'COMMAND=install; do_install' 2>&1) || fail "the install under $fw failed: $out"
        # shellcheck disable=SC2086
        [ "$(fw_rules)" = "$(sorted $clients 3375/tcp)" ] || fail "a plain install under $fw opened: $(fw_rules)"
        printf '%s' "$out" | grep -q "Opened in $fw: " || fail "the install does not say what it opened in $fw: $out"
        first=$(grep -nE '^(ufw allow|firewall-cmd --permanent --add-port)' "$CALLS" | head -n 1 | cut -d: -f1)
        up=$(grep -n '^compose up' "$CALLS" | head -n 1 | cut -d: -f1)
        [ -n "$first" ] && [ "$first" -lt "$up" ] || fail "under $fw the ports were opened after the start: $(cat "$CALLS")"

        : > "$CALLS"
        out=$(mgr 'COMMAND=install; do_install' 2>&1) || fail "the second run under $fw failed: $out"
        if grep -qE '^(ufw allow|firewall-cmd --permanent --add-port)' "$CALLS"; then fail "the second run under $fw opened ports again: $(cat "$CALLS")"; fi

        env_set HTTPS_ENABLED true
        mgr 'COMMAND=update; do_install' > /dev/null 2>&1 || fail "the update with HTTPS under $fw failed"
        # shellcheck disable=SC2086
        [ "$(fw_rules)" = "$(sorted $clients 3375/tcp 5443/tcp)" ] || fail "HTTPS under $fw opened: $(fw_rules)"

        env_set TRUST_PROXY 1
        out=$(mgr 'COMMAND=update; do_install' 2>&1) || fail "the update with TRUST_PROXY under $fw failed: $out"
        # shellcheck disable=SC2086
        [ "$(fw_rules)" = "$(sorted $clients)" ] || fail "behind a reverse proxy $fw still lets in: $(fw_rules)"
        printf '%s' "$out" | grep -q "Closed in $fw: " || fail "the update does not say what it closed in $fw: $out"
        case "$fw" in
            ufw) printf '%s' "$out" | grep -q 'sudo ufw allow 3375/tcp' || fail "no hint how to open 3375 in ufw: $out" ;;
            firewalld) printf '%s' "$out" | grep -q 'firewall-cmd --permanent --add-port=3375/tcp' || fail "no hint how to open 3375 in firewalld: $out" ;;
        esac

        out=$(mgr 'do_uninstall' 2>&1) || fail "the uninstall under $fw failed: $out"
        [ -z "$(fw_rules)" ] || fail "the uninstall left in $fw: $(fw_rules)"
        printf '%s' "$out" | grep -q "Closed in $fw: " || fail "the uninstall does not say what it closed in $fw: $out"
        rm -rf "$TEST_DIR"
    done
)

# With Traefik the firewall lets in 80 and 443 instead of the console's
# port, and --no-traefik closes them again. A rule that was there before
# (a web server's 443) is not added twice and stays; rules of install.sh
# before 3.6.23 (comment "NeoDesk ...", no record) close at uninstall.
test_firewall_with_traefik_and_old_rules() (
    load
    local out
    printf 'ufw\n' > "$TEST_DIR/fw"
    printf '%-26s ALLOW       Anywhere                   # web server\n' 443/tcp > "$TEST_DIR/fw-rules"
    printf '%-26s ALLOW       Anywhere                   # NeoDesk signal UDP\n' 21116/udp >> "$TEST_DIR/fw-rules"
    out=$(mgr 'getent() { return 2; }
               DOMAIN_SET=console.example.com; LETSENCRYPT_EMAIL_SET=ops@neodesk-test.de; TRAEFIK_ACTION=on
               COMMAND=install; do_install' 2>&1) || fail "the install with --domain failed: $out"
    case " $(fw_rules) " in *" 80/tcp "*) ;; *) fail "Traefik's 80 is not open: $(fw_rules)" ;; esac
    case " $(fw_rules) " in *" 3375/tcp "*) fail "the console's own port is open behind Traefik: $(fw_rules)" ;; esac
    if grep -q '^ufw allow 443/tcp' "$CALLS"; then fail "443 was added although it was open: $(cat "$CALLS")"; fi
    out=$(mgr 'TRAEFIK_ACTION=off; COMMAND=update; do_install' 2>&1) || fail "--no-traefik failed: $out"
    case " $(fw_rules) " in *" 80/tcp "*) fail "--no-traefik left 80 open: $(fw_rules)" ;; esac
    case " $(fw_rules) " in *" 443/tcp "*) ;; *) fail "--no-traefik closed the web server's 443: $(fw_rules)" ;; esac
    case " $(fw_rules) " in *" 3375/tcp "*) ;; *) fail "--no-traefik did not open the console's port: $(fw_rules)" ;; esac
    rm -f "$NEODESK_DIR/.firewall"
    mgr 'do_uninstall' > /dev/null 2>&1 || fail "the uninstall failed"
    [ "$(fw_rules)" = "443/tcp " ] || fail "the uninstall did not close the old rules, or closed the web server's: $(fw_rules)"
    rm -rf "$TEST_DIR"
)

# An installed ufw that is off gets no rules (they would go live with the
# next "ufw enable"), and --skip-firewall leaves an active one alone.
test_firewall_left_alone() (
    load
    mgr 'COMMAND=install; do_install' > /dev/null 2>&1 || fail "the install failed"
    if grep -qE '^ufw (allow|delete)|^firewall-cmd --permanent' "$CALLS"; then fail "an inactive ufw was changed: $(cat "$CALLS")"; fi
    rm -rf "$TEST_DIR"
    load
    printf 'ufw\n' > "$TEST_DIR/fw"
    mgr 'parse_args --skip-firewall; COMMAND=install; do_install' > /dev/null 2>&1 || fail "the install with --skip-firewall failed"
    if grep -q '^ufw' "$CALLS"; then fail "--skip-firewall touched ufw: $(cat "$CALLS")"; fi
    rm -rf "$TEST_DIR"
)

# MIGRATE-CLIENT-9: --import-keys takes a RustDesk server's key pair
# instead of a new one: it is checked first (both files, the halves
# belong together), goes into the data volume before the first start,
# and on an install that runs, the update does not stop at "up to date"
# but stops NeoDesk, backs it up and imports the key before the start.
test_import_keys() (
    load
    local out keys seed pubkey import up stop
    keys="$TEST_DIR/rustdesk"
    mkdir -p "$keys"
    if out=$(mgr "parse_args --import-keys $keys" 2>&1); then fail "--import-keys without key files went on"; fi
    printf '%s' "$out" | grep -q 'id_ed25519' || fail "the error does not name the files: $out"
    seed=$(head -c 32 /dev/urandom | base64 -w0)
    pubkey=$(head -c 32 /dev/urandom | base64 -w0)
    { printf '%s' "$seed" | base64 -d; printf '%s' "$pubkey" | base64 -d; } | base64 -w0 > "$keys/id_ed25519"
    head -c 32 /dev/urandom | base64 -w0 > "$keys/id_ed25519.pub"
    if out=$(mgr "parse_args --import-keys $keys" 2>&1); then fail "a public key of another pair went on"; fi
    printf '%s' "$out" | grep -q 'does not belong' || fail "the mismatch error does not say so: $out"
    printf '%s' "$pubkey" > "$keys/id_ed25519.pub"
    out=$(mgr "parse_args --import-keys $keys; COMMAND=install; do_install" 2>&1) || fail "the install with --import-keys failed: $out"
    import=$(grep -n "^run --rm --entrypoint sh --user 0 -v neodesk-data:/data -v $keys:/in:ro" "$CALLS" | head -n 1 | cut -d: -f1)
    up=$(grep -n '^compose up' "$CALLS" | head -n 1 | cut -d: -f1)
    [ -n "$import" ] && [ "$import" -lt "$up" ] || fail "the key was not imported before the first start: $(cat "$CALLS")"
    printf '%s' "$out" | grep -q 'imported' || fail "the install does not say it imported the key: $out"

    : > "$CALLS"
    out=$(mgr "parse_args --import-keys $keys; COMMAND=install; do_install" 2>&1) || fail "the import on a running install failed: $out"
    if printf '%s' "$out" | grep -q 'up to date'; then fail "the import on a running install stopped at up to date: $out"; fi
    stop=$(grep -n '^compose stop' "$CALLS" | head -n 1 | cut -d: -f1)
    import=$(grep -n "^run --rm --entrypoint sh --user 0 -v neodesk-data:/data" "$CALLS" | head -n 1 | cut -d: -f1)
    up=$(grep -n '^compose up' "$CALLS" | head -n 1 | cut -d: -f1)
    { [ -n "$stop" ] && [ -n "$import" ] && [ -n "$up" ] && [ "$stop" -lt "$import" ] && [ "$import" -lt "$up" ]; } \
        || fail "the import did not stop NeoDesk, import and start it, in that order: $(cat "$CALLS")"
    rm -rf "$TEST_DIR"
)

# reset-password gives the account a new password through the server's
# reset-password; --reset-two-factor passes the option on (it also turns
# the account's two-factor sign-in off), and without it nothing else is
# passed. An image that refuses the option gets a hint to update.
test_reset_password_two_factor() (
    load
    mgr 'COMMAND=install; do_install' > /dev/null 2>&1 || fail "the install failed"
    local out
    : > "$CALLS"
    out=$(mgr 'parse_args reset-password; do_reset_password' 2>&1) || fail "reset-password failed: $out"
    grep -q 'neodesk-server reset-password admin$' "$CALLS" || fail "reset-password did not run the server's command for admin: $(cat "$CALLS")"
    if grep -q -- '--reset-two-factor' "$CALLS"; then fail "reset-password turned two-factor sign-in off unasked: $(cat "$CALLS")"; fi
    if printf '%s' "$out" | grep -q 'Two-factor'; then fail "reset-password talks of two-factor sign-in unasked: $out"; fi
    : > "$CALLS"
    out=$(mgr 'parse_args reset-password --reset-two-factor --user ops; do_reset_password' 2>&1) || fail "reset-password --reset-two-factor failed: $out"
    grep -q 'neodesk-server reset-password --reset-two-factor ops$' "$CALLS" || fail "--reset-two-factor was not passed on: $(cat "$CALLS")"
    printf '%s' "$out" | grep -q 'Two-factor sign-in of ops is off' || fail "the output does not say two-factor sign-in is off: $out"
    if out=$(mgr 'docker() { if [ "$1" = exec ] && [[ "$*" == *--reset-two-factor* ]]; then cat > /dev/null; return 2; fi; command_docker "$@"; }
                  parse_args reset-password --reset-two-factor; do_reset_password' 2>&1); then
        fail "a refused --reset-two-factor passed"
    fi
    printf '%s' "$out" | grep -q 'update first' || fail "a refused --reset-two-factor gives no hint: $out"
    rm -rf "$TEST_DIR"
)

test_fresh_install
test_no_release_image_runs_dev
test_update_goes_back_when_unhealthy
test_update_backs_up_the_files_from_before
test_data_volume_at_the_neodesk_path
test_update_from_the_old_data_path
test_old_backup_restores
test_update_keeps_compose_edits
test_fresh_install_on_postgres
test_foreign_install_is_left_alone
test_errors_say_what_to_do
test_uninstall_says_what_it_leaves
test_purge_dumps_postgres_first
test_help_lists_every_option
test_postgres_switch_moves_the_data
test_postgres_declined_keeps_sqlite
test_postgres_failed_move_keeps_sqlite
test_failed_first_install_resumes
test_rerun_without_changes_keeps_the_container
test_summary_names_the_console_ports_it_serves
test_install_sh_runs_the_manager
test_install_sh_help_when_piped
test_firewall_follows_what_it_serves
test_firewall_with_traefik_and_old_rules
test_firewall_left_alone
test_import_keys
test_reset_password_two_factor
test_traefik_with_letsencrypt
test_traefik_errors_say_what_to_do
test_install_sh_passes_the_domain
test_staging_needs_a_staging_certificate
test_staging_to_production_renews
test_domain_change_remakes_traefik
test_dns_check_knows_ipv6
test_new_install_gets_ipv6
test_files_come_from_the_release
test_install_sh_takes_the_release_files
test_update_moves_the_default_console_port
echo "Docker installer checks passed"
bash "$SCRIPT_ROOT/scripts/test-docker-entrypoint.sh"
