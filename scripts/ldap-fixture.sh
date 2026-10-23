#!/usr/bin/env bash
# OpenLDAP fixture for the auth integration lanes (identity-and-access E1b).
#
#   scripts/ldap-fixture.sh tls <dir>
#       Write a throwaway CA (ca.crt) and a directory certificate and key
#       (server.crt, server.key) for localhost and 127.0.0.1 into <dir>. The
#       Caesium server trusts ca.crt through CAESIUM_AUTH_LDAP_CA_FILE, so the
#       lanes verify the directory's certificate exactly as production does.
#
#   scripts/ldap-fixture.sh run <container-cli> <name> <server-container> <tls-dir> [extra run args...]
#       Start the directory in the server container's network namespace
#       (server, runner and directory all reach it at localhost:636), seeded
#       from test/testdata/ldap/bootstrap.ldif. Image: CAESIUM_LDAP_IMAGE, or
#       the pinned default below.
#
#   scripts/ldap-fixture.sh wait <container-cli> <name>
#       Wait until the directory answers an LDAPS search for the seeded group
#       base, verifying its certificate against the fixture CA.
#
# The admin password and every key here are fixture material for a container
# that lives only as long as one lane run.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEFAULT_IMAGE="osixia/openldap:1.5.0@sha256:18742e9c449c9c1afe129d3f2f3ee15fb34cc43e5f940a20f3399728f41d7c28"
ADMIN_DN="cn=admin,dc=example,dc=org"
ADMIN_PASSWORD="iaa-ldap-admin-secret"
CERT_DIR="/container/service/slapd/assets/certs"
LDIF_DIR="/container/service/slapd/assets/config/bootstrap/ldif/custom"

tls() {
    local dir="${1:?usage: ldap-fixture.sh tls <dir>}"
    rm -rf "$dir"
    mkdir -p "$dir"
    # RFC 7919's public FFDHE2048 group avoids random-prime generation at
    # directory startup. These standard DH parameters contain no private key.
    cp "$ROOT/test/testdata/ldap/dhparam.pem" "$dir/dhparam.pem"
    local work
    work="$(mktemp -d "${TMPDIR:-/tmp}/caesium-ldap-tls.XXXXXX")"
    trap 'rm -rf "$work"' RETURN
    openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
        -subj '/CN=caesium integration LDAP CA' \
        -addext basicConstraints=critical,CA:TRUE \
        -addext keyUsage=critical,keyCertSign,cRLSign \
        -keyout "$work/ca.key" -out "$dir/ca.crt" >/dev/null 2>&1
    openssl req -newkey rsa:2048 -nodes -subj '/CN=localhost' \
        -keyout "$dir/server.key" -out "$work/server.csr" >/dev/null 2>&1
    printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth\nkeyUsage=critical,digitalSignature,keyEncipherment\n' >"$work/ext.cnf"
    openssl x509 -req -in "$work/server.csr" -CA "$dir/ca.crt" -CAkey "$work/ca.key" \
        -CAcreateserial -days 2 -extfile "$work/ext.cnf" -out "$dir/server.crt" >/dev/null 2>&1
    openssl verify -CAfile "$dir/ca.crt" "$dir/server.crt" >/dev/null
    # The directory's own user must read the key inside its container, and
    # the coverage server (UID 10001) the CA.
    chmod 0755 "$dir"
    chmod 0644 "$dir"/*
}

run() {
    local cli="${1:?usage: ldap-fixture.sh run <cli> <name> <server> <tls-dir> [args...]}"
    local name="${2:?}" server="${3:?}" tlsdir="${4:?}"
    shift 4
    "$cli" run -d --name "$name" \
        --network "container:$server" \
        "$@" \
        -v "$tlsdir:$CERT_DIR:ro" \
        -v "$ROOT/test/testdata/ldap:$LDIF_DIR:ro" \
        -e LDAP_ORGANISATION=Caesium \
        -e LDAP_DOMAIN=example.org \
        -e LDAP_ADMIN_PASSWORD="$ADMIN_PASSWORD" \
        -e LDAP_TLS=true \
        -e LDAP_TLS_CRT_FILENAME=server.crt \
        -e LDAP_TLS_KEY_FILENAME=server.key \
        -e LDAP_TLS_CA_CRT_FILENAME=ca.crt \
        -e LDAP_TLS_DH_PARAM_FILENAME=dhparam.pem \
        -e LDAP_TLS_VERIFY_CLIENT=never \
        -e LDAP_TLS_ENFORCE=false \
        "${CAESIUM_LDAP_IMAGE:-$DEFAULT_IMAGE}" --copy-service
}

wait_ready() {
    local cli="${1:?usage: ldap-fixture.sh wait <cli> <name>}" name="${2:?}"
    # Each probe is bounded inside the container (timeout + LDAP network and
    # server time limits), and the whole wait by an elapsed deadline, so a slapd
    # that accepts and then stalls in TLS, bind or search cannot hang the lane.
    # This libldap canonicalizes localhost to the container hostname. Use the
    # loopback IP covered by the certificate SAN for verified readiness.
    local deadline=$((SECONDS + 90))
    until "$cli" exec -e LDAPTLS_CACERT="$CERT_DIR/ca.crt" -e LDAPTLS_REQCERT=demand "$name" \
        timeout 5 ldapsearch -o nettimeout=5 -l 5 -x -H ldaps://127.0.0.1:636 \
        -D "$ADMIN_DN" -w "$ADMIN_PASSWORD" \
        -b ou=groups,dc=example,dc=org -s base dn >/dev/null 2>&1; do
        if [ "$SECONDS" -ge "$deadline" ]; then
            echo "OpenLDAP fixture $name did not answer LDAPS within 90s" >&2
            "$cli" logs --tail 60 "$name" >&2 || true
            return 1
        fi
        running="$("$cli" inspect -f '{{.State.Running}}' "$name" 2>/dev/null || true)"
        if [ "$running" != "true" ]; then
            echo "OpenLDAP fixture $name exited" >&2
            "$cli" logs --tail 60 "$name" >&2 || true
            return 1
        fi
        sleep 1
    done
}

cmd="${1:-}"
shift || true
case "$cmd" in
    tls) tls "$@" ;;
    run) run "$@" ;;
    wait) wait_ready "$@" ;;
    *)
        echo "usage: ldap-fixture.sh tls|run|wait ..." >&2
        exit 2
        ;;
esac
