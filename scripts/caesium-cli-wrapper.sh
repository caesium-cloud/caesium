#!/usr/bin/env sh
# Generate and run the container CLI wrapper used by `just cli`.
#
# The generated wrapper is self-contained (copied into .tmp/caesium-cli/caesium)
# so it still works if this repo tree is not present. Socket lookup, kubeconfig
# mounts, and user/group selection happen at wrapper *runtime*, not generation.

# --- caesium-cli-runtime ---
caesium_cli_quote() {
    printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

caesium_cli_docker_reset() {
    CAESIUM_CLI_DOCKER_ARGS=""
}

caesium_cli_docker_add() {
    while [ $# -gt 0 ]; do
        CAESIUM_CLI_DOCKER_ARGS="$CAESIUM_CLI_DOCKER_ARGS $(caesium_cli_quote "$1")"
        shift
    done
}

# True when the octal mode's group bits include write (2/3/6/7).
caesium_cli_mode_group_writable() {
    m=$1
    while [ ${#m} -gt 3 ]; do
        m=${m#?}
    done
    while [ ${#m} -lt 3 ]; do
        m="0$m"
    done
    g=$(printf '%s' "$m" | cut -c2)
    case $g in
        2|3|6|7) return 0 ;;
        *) return 1 ;;
    esac
}

# Print docker --user/--group-add flags for a resolved socket.
# Args: host_uid host_gid sock_uid sock_gid sock_mode sock_writable(1|0)
caesium_cli_choose_user_flags() {
    host_uid=$1
    host_gid=$2
    sock_uid=$3
    sock_gid=$4
    sock_mode=$5
    sock_writable=$6
    if [ "$sock_uid" = "$host_uid" ] && [ "$sock_writable" = "1" ]; then
        printf '%s' "--user=${host_uid}:${host_gid}"
        return 0
    fi
    if caesium_cli_mode_group_writable "$sock_mode"; then
        printf '%s' "--user=${host_uid}:${host_gid} --group-add=${sock_gid}"
        return 0
    fi
    printf '%s' "--user=0:0"
}

# Host socket ownership is not the Docker Desktop VM's. Probe whether a
# container running with the candidate --user/--group-add flags can write
# the mounted socket. CAESIUM_CLI_SOCKET_IN_CONTAINER_WRITABLE=0|1 stubs
# the probe for hermetic tests.
# Remaining args after image/cli are docker user flags.
caesium_cli_container_can_write_socket() {
    sock=$1
    image=$2
    cli=$3
    shift 3
    case ${CAESIUM_CLI_SOCKET_IN_CONTAINER_WRITABLE:-} in
        0|false|no) return 1 ;;
        1|true|yes) return 0 ;;
    esac
    "$cli" run --rm \
        -v "${sock}:/var/run/docker.sock" \
        "$@" \
        --entrypoint /bin/sh \
        "$image" \
        -c 'test -w /var/run/docker.sock' >/dev/null 2>&1
}

caesium_cli_stat_ids() {
    path=$1
    CAESIUM_CLI_STAT_UID=
    CAESIUM_CLI_STAT_GID=
    CAESIUM_CLI_STAT_MODE=
    if CAESIUM_CLI_STAT_UID=$(stat -f '%u' "$path" 2>/dev/null); then
        CAESIUM_CLI_STAT_GID=$(stat -f '%g' "$path")
        CAESIUM_CLI_STAT_MODE=$(stat -f '%OLp' "$path")
        return 0
    fi
    if CAESIUM_CLI_STAT_UID=$(stat -c '%u' "$path" 2>/dev/null); then
        CAESIUM_CLI_STAT_GID=$(stat -c '%g' "$path")
        CAESIUM_CLI_STAT_MODE=$(stat -c '%a' "$path")
        return 0
    fi
    return 1
}

caesium_cli_unix_path_from_docker_host() {
    case ${1:-} in
        unix://*)
            printf '%s' "${1#unix://}"
            return 0
            ;;
        *)
            return 1
            ;;
    esac
}

# Sets CAESIUM_CLI_RESOLVED_SOCK, CAESIUM_CLI_RESOLVED_SOCK_MISSING,
# CAESIUM_CLI_DOCKER_HOST_PASSTHROUGH.
caesium_cli_resolve_socket() {
    CAESIUM_CLI_RESOLVED_SOCK=
    CAESIUM_CLI_RESOLVED_SOCK_MISSING=
    CAESIUM_CLI_DOCKER_HOST_PASSTHROUGH=

    if [ -n "${DOCKER_HOST:-}" ]; then
        if sock=$(caesium_cli_unix_path_from_docker_host "$DOCKER_HOST"); then
            CAESIUM_CLI_RESOLVED_SOCK=$sock
            if [ -S "$sock" ]; then
                return 0
            fi
            CAESIUM_CLI_RESOLVED_SOCK_MISSING=1
            return 1
        fi
        CAESIUM_CLI_DOCKER_HOST_PASSTHROUGH=$DOCKER_HOST
        return 0
    fi

    if [ -n "${CAESIUM_SOCK:-}" ]; then
        CAESIUM_CLI_RESOLVED_SOCK=$CAESIUM_SOCK
        if [ -S "$CAESIUM_SOCK" ]; then
            return 0
        fi
        CAESIUM_CLI_RESOLVED_SOCK_MISSING=1
        return 1
    fi

    podman=${CAESIUM_PODMAN:-${CAESIUM_CLI_DEFAULT_PODMAN:-false}}
    if [ "$podman" = "true" ]; then
        if [ -n "${XDG_RUNTIME_DIR:-}" ] && [ -S "$XDG_RUNTIME_DIR/podman/podman.sock" ]; then
            CAESIUM_CLI_RESOLVED_SOCK=$XDG_RUNTIME_DIR/podman/podman.sock
            return 0
        fi
        if [ -S "/run/user/$(id -u)/podman/podman.sock" ]; then
            CAESIUM_CLI_RESOLVED_SOCK=/run/user/$(id -u)/podman/podman.sock
            return 0
        fi
        if [ -n "${XDG_RUNTIME_DIR:-}" ]; then
            CAESIUM_CLI_RESOLVED_SOCK=$XDG_RUNTIME_DIR/podman/podman.sock
        else
            CAESIUM_CLI_RESOLVED_SOCK=/run/user/$(id -u)/podman/podman.sock
        fi
        CAESIUM_CLI_RESOLVED_SOCK_MISSING=1
        return 1
    fi

    if [ -n "${HOME:-}" ] && [ -S "$HOME/.docker/run/docker.sock" ]; then
        CAESIUM_CLI_RESOLVED_SOCK=$HOME/.docker/run/docker.sock
        return 0
    fi
    if [ -n "${HOME:-}" ] && [ -S "$HOME/.docker/desktop/docker.sock" ]; then
        CAESIUM_CLI_RESOLVED_SOCK=$HOME/.docker/desktop/docker.sock
        return 0
    fi
    if [ -S /var/run/docker.sock ]; then
        CAESIUM_CLI_RESOLVED_SOCK=/var/run/docker.sock
        return 0
    fi
    CAESIUM_CLI_RESOLVED_SOCK=/var/run/docker.sock
    CAESIUM_CLI_RESOLVED_SOCK_MISSING=1
    return 1
}

# Commands that launch task containers or otherwise talk to a local runtime.
caesium_cli_needs_runtime() {
    for arg in "$@"; do
        case $arg in
            -h|--help|help) return 1 ;;
        esac
    done
    cmd=
    for arg in "$@"; do
        case $arg in
            --) continue ;;
            -*) continue ;;
            *)
                cmd=$arg
                break
                ;;
        esac
    done
    case $cmd in
        dev|reproduce|start) return 0 ;;
        test)
            for arg in "$@"; do
                case $arg in
                    --check-images|--check-images=*|--scenario|--scenario=*)
                        return 0
                        ;;
                esac
            done
            return 1
            ;;
        *) return 1 ;;
    esac
}

caesium_cli_kubeconfig_src() {
    CAESIUM_CLI_KUBECONFIG_SRC=
    if [ -n "${KUBECONFIG:-}" ]; then
        src=${KUBECONFIG%%:*}
        if [ -f "$src" ]; then
            CAESIUM_CLI_KUBECONFIG_SRC=$src
            return 0
        fi
        return 1
    fi
    if [ -n "${CAESIUM_KUBERNETES_CONFIG:-}" ] && [ -f "${CAESIUM_KUBERNETES_CONFIG}/.kube/config" ]; then
        CAESIUM_CLI_KUBECONFIG_SRC=${CAESIUM_KUBERNETES_CONFIG}/.kube/config
        return 0
    fi
    if [ -n "${HOME:-}" ] && [ -f "$HOME/.kube/config" ]; then
        CAESIUM_CLI_KUBECONFIG_SRC=$HOME/.kube/config
        return 0
    fi
    return 1
}

# Use the Kubernetes parser offline: it understands YAML/JSON scalars and
# resolves certificate paths relative to the source config and validates
# their data fields. No API requests or exec auth plugins run.
caesium_cli_flatten_kubeconfig() {
    if ! command -v kubectl >/dev/null 2>&1; then
        printf '%s\n' "error: kubectl is required on the host when sharing a kubeconfig." >&2
        return 1
    fi
    (
        umask 077
        kubectl --kubeconfig="$1" config view --raw --flatten -o json > "$2"
    )
}

caesium_cli_cleanup_kubeconfig() {
    if [ -n "${CAESIUM_CLI_KUBE_TMPDIR:-}" ]; then
        rm -f "$CAESIUM_CLI_KUBE_TMPDIR/kubeconfig"
        rmdir "$CAESIUM_CLI_KUBE_TMPDIR"
        CAESIUM_CLI_KUBE_TMPDIR=
    fi
}

caesium_cli_prepare_kubeconfig() {
    # Never reuse a per-user path: concurrent commands may target different
    # clusters. mktemp creates a private directory without following symlinks.
    CAESIUM_CLI_KUBE_TMPDIR=$(mktemp -d "${TMPDIR:-/tmp}/caesium-cli.XXXXXXXX") || return 1
    trap 'caesium_cli_cleanup_kubeconfig' 0
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 143' TERM
    caesium_cli_flatten_kubeconfig "$1" "$CAESIUM_CLI_KUBE_TMPDIR/kubeconfig"
}

caesium_cli_forward_signal() {
    kill -s "$1" "$CAESIUM_CLI_CHILD" 2>/dev/null || true
}

caesium_cli_socket_error() {
    sock=${1:-/var/run/docker.sock}
    printf '%s\n' "error: container runtime socket not found at ${sock}." >&2
    printf '%s\n' "       Start Docker Desktop (or the Docker/Podman daemon) and retry." >&2
    printf '%s\n' "       Or set DOCKER_HOST=unix:///path/to/docker.sock or CAESIUM_SOCK=/path/to/docker.sock" >&2
    printf '%s\n' "       to the active socket (Docker Desktop often uses \$HOME/.docker/run/docker.sock)." >&2
}

caesium_cli_run() {
    CAESIUM_CLI_KUBE_TMPDIR=
    image=${CAESIUM_CLI_IMAGE:?CAESIUM_CLI_IMAGE is not set}
    container_cli=${CAESIUM_CONTAINER_CLI:-${CAESIUM_CLI_CONTAINER_CLI:-docker}}
    podman=${CAESIUM_PODMAN:-${CAESIUM_CLI_DEFAULT_PODMAN:-false}}

    needs_runtime=0
    if caesium_cli_needs_runtime "$@"; then
        needs_runtime=1
    fi

    if ! caesium_cli_resolve_socket; then
        :
    fi

    if [ "$needs_runtime" = "1" ] && [ -z "${CAESIUM_CLI_DOCKER_HOST_PASSTHROUGH:-}" ]; then
        if [ -n "${CAESIUM_CLI_RESOLVED_SOCK_MISSING:-}" ] || [ -z "${CAESIUM_CLI_RESOLVED_SOCK:-}" ]; then
            caesium_cli_socket_error "${CAESIUM_CLI_RESOLVED_SOCK:-/var/run/docker.sock}"
            return 1
        fi
    fi

    caesium_cli_docker_reset
    caesium_cli_docker_add run --rm

    if [ "$(uname -s)" = "Linux" ]; then
        caesium_cli_docker_add --network host
    else
        caesium_cli_docker_add --add-host host.docker.internal:host-gateway
    fi

    # `for x in $empty` is a dash syntax error; iterate via here-doc instead.
    while IFS= read -r name; do
        [ -n "$name" ] || continue
        caesium_cli_docker_add -e "$name"
    done <<EOF
$(env | sed -n 's/^\(CAESIUM_[A-Za-z0-9_]*\)=.*/\1/p')
EOF

    CAESIUM_CLI_AS_ROOT=0
    host_uid=$(id -u)
    host_gid=$(id -g)
    if [ -n "${CAESIUM_CLI_RESOLVED_SOCK:-}" ] && [ -z "${CAESIUM_CLI_RESOLVED_SOCK_MISSING:-}" ]; then
        caesium_cli_docker_add -v "${CAESIUM_CLI_RESOLVED_SOCK}:/var/run/docker.sock"
        caesium_cli_docker_add -e DOCKER_HOST=unix:///var/run/docker.sock
        caesium_cli_docker_add -e CAESIUM_SOCK=/var/run/docker.sock
        if [ "$podman" = "true" ]; then
            caesium_cli_docker_add -e CAESIUM_PODMAN_URI=unix:///var/run/docker.sock
        fi
        writable=0
        if [ -w "$CAESIUM_CLI_RESOLVED_SOCK" ]; then
            writable=1
        fi
        if ! caesium_cli_stat_ids "$CAESIUM_CLI_RESOLVED_SOCK"; then
            CAESIUM_CLI_STAT_UID=$host_uid
            CAESIUM_CLI_STAT_GID=$host_gid
            CAESIUM_CLI_STAT_MODE=600
        fi
        user_flags=$(caesium_cli_choose_user_flags \
            "$host_uid" "$host_gid" \
            "$CAESIUM_CLI_STAT_UID" "$CAESIUM_CLI_STAT_GID" \
            "$CAESIUM_CLI_STAT_MODE" "$writable")
        # Docker Desktop presents a user-owned host socket but remaps it to
        # 0:0 mode 0660 inside the Linux VM. Probe with the candidate flags
        # and fall back to root when the container cannot write the mount.
        case $user_flags in
            --user=0:0) ;;
            *)
                if ! caesium_cli_container_can_write_socket \
                    "$CAESIUM_CLI_RESOLVED_SOCK" "$image" "$container_cli" \
                    $user_flags; then
                    user_flags="--user=0:0"
                fi
                ;;
        esac
        # Flags are tokens without whitespace (--user=uid:gid, --group-add=gid).
        # Word-splitting here must not use `set --`, which would clobber CLI argv.
        for flag in $user_flags; do
            caesium_cli_docker_add "$flag"
            case $flag in
                --user=0:0) CAESIUM_CLI_AS_ROOT=1 ;;
            esac
        done
    else
        caesium_cli_docker_add "--user=${host_uid}:${host_gid}"
        if [ -n "${CAESIUM_CLI_DOCKER_HOST_PASSTHROUGH:-}" ]; then
            caesium_cli_docker_add -e "DOCKER_HOST=${CAESIUM_CLI_DOCKER_HOST_PASSTHROUGH}"
        fi
    fi

    if caesium_cli_kubeconfig_src; then
        # Layout getKubernetesCore expects: $CAESIUM_KUBERNETES_CONFIG/.kube/config.
        # Flatten file-referenced certs/keys first so relocating the config does
        # not leave certificate-authority: ca.crt pointing at a missing mount.
        kube_root=/caesium-kube
        kube_dest=${kube_root}/.kube/config
        if ! caesium_cli_prepare_kubeconfig "$CAESIUM_CLI_KUBECONFIG_SRC"; then
            return 1
        fi
        caesium_cli_docker_add -v "${CAESIUM_CLI_KUBE_TMPDIR}/kubeconfig:${kube_dest}:ro"
        caesium_cli_docker_add -e "KUBECONFIG=${kube_dest}"
        caesium_cli_docker_add -e "CAESIUM_KUBERNETES_CONFIG=${kube_root}"
    fi

    caesium_cli_docker_add -v "${PWD}:/work" -w /work
    caesium_cli_docker_add --entrypoint /bin/caesium
    caesium_cli_docker_add "$image"

    user_q=
    for a in "$@"; do
        user_q="$user_q $(caesium_cli_quote "$a")"
    done
    if [ -z "$CAESIUM_CLI_KUBE_TMPDIR" ]; then
        eval "exec $(caesium_cli_quote "$container_cli") $CAESIUM_CLI_DOCKER_ARGS $user_q"
    fi

    # Keep the parent alive to own the temporary config for the complete
    # container lifetime. Forward signals and do not clean up on an interrupted
    # wait until the child really exits. Preserve its status and stdin.
    # dash redirects stdin for asynchronous lists before applying <&0. Save
    # the original descriptor first so piped input survives that redirection.
    exec 3<&0
    eval "exec $(caesium_cli_quote "$container_cli") $CAESIUM_CLI_DOCKER_ARGS $user_q" <&3 3<&- &
    CAESIUM_CLI_CHILD=$!
    exec 3<&-
    trap 'caesium_cli_forward_signal HUP' HUP
    trap 'caesium_cli_forward_signal INT' INT
    trap 'caesium_cli_forward_signal TERM' TERM
    while :; do
        if wait "$CAESIUM_CLI_CHILD"; then
            child_status=0
            break
        else
            child_status=$?
        fi
        if ! kill -0 "$CAESIUM_CLI_CHILD" 2>/dev/null; then
            break
        fi
    done
    return "$child_status"
}
# --- caesium-cli-runtime-end ---

caesium_cli_generate() {
    output=
    image=
    container_cli=docker
    podman=false
    tag=
    while [ $# -gt 0 ]; do
        case $1 in
            --output)
                output=$2
                shift 2
                ;;
            --image)
                image=$2
                shift 2
                ;;
            --container-cli)
                container_cli=$2
                shift 2
                ;;
            --podman)
                podman=$2
                shift 2
                ;;
            --tag)
                tag=$2
                shift 2
                ;;
            *)
                printf '%s\n' "error: unknown generate argument: $1" >&2
                return 2
                ;;
        esac
    done
    if [ -z "$output" ] || [ -z "$image" ]; then
        printf '%s\n' "error: generate requires --output PATH --image IMAGE" >&2
        return 2
    fi

    self=$0
    case $self in
        /*) ;;
        *) self=$(pwd)/$self ;;
    esac
    if [ ! -f "$self" ]; then
        printf '%s\n' "error: cannot locate wrapper source at $self" >&2
        return 1
    fi

    mkdir -p "$(dirname "$output")"
    tag_label=${tag:-unknown}
    {
        printf '%s\n' '#!/usr/bin/env sh'
        printf '%s\n' "# Generated by \`just tag=${tag_label} cli\`. Runs the Caesium CLI from"
        printf '%s\n' "# ${image} against the current directory."
        printf '%s\n' "#"
        printf '%s\n' "# At runtime this wrapper mounts the host Docker/Podman socket (see"
        printf '%s\n' "# DOCKER_HOST / CAESIUM_SOCK) so \`caesium dev\`, harness scenarios,"
        printf '%s\n' "# --check-images, and reproduce can talk to the daemon. If the socket"
        printf '%s\n' "# is not writable *inside the CLI container* (Docker Desktop remaps"
        printf '%s\n' "# a user-owned host socket to 0:0), the wrapper probes and falls back"
        printf '%s\n' "# to root — that is full daemon access. When KUBECONFIG,"
        printf '%s\n' "# CAESIUM_KUBERNETES_CONFIG/.kube/config, or ~/.kube/config exists, it"
        printf '%s\n' "# is flattened with host kubectl into a private temporary file and mounted"
        printf '%s\n' "# at /caesium-kube/.kube/config, and CAESIUM_KUBERNETES_CONFIG is set"
        printf '%s\n' "# to /caesium-kube so kubernetes steps in \`caesium dev\` load it"
        printf '%s\n' "# (host credential sharing). The copy is removed when the container exits."
        printf '%s\n' "CAESIUM_CLI_IMAGE=$(caesium_cli_quote "$image")"
        printf '%s\n' "CAESIUM_CLI_CONTAINER_CLI=$(caesium_cli_quote "$container_cli")"
        printf '%s\n' "CAESIUM_CLI_DEFAULT_PODMAN=$(caesium_cli_quote "$podman")"
        printf '%s\n' 'set -e'
        sed -n '/^# --- caesium-cli-runtime ---/,/^# --- caesium-cli-runtime-end ---/p' "$self"
        printf '%s\n' 'caesium_cli_run "$@"'
    } >"$output"
    chmod +x "$output"
}

if [ -n "${CAESIUM_CLI_WRAPPER_SOURCED:-}" ]; then
    return 0 2>/dev/null || exit 0
fi

set -e

if [ "${1:-}" = "generate" ]; then
    shift
    caesium_cli_generate "$@"
    exit $?
fi

if [ -n "${CAESIUM_CLI_IMAGE:-}" ]; then
    caesium_cli_run "$@"
    exit $?
fi

printf '%s\n' "usage: $0 generate --output PATH --image IMAGE [--container-cli docker] [--podman false] [--tag TAG]" >&2
exit 2
