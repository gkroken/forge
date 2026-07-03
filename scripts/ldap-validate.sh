#!/usr/bin/env bash
# ldap-validate.sh — end-to-end live validation of forge's direct LDAP/AD login.
#
# Spins up an OpenLDAP container seeded with three users and two groups, starts
# forge with -auth + -ldap-*, then drives the login form with curl and asserts
# that group→role mapping produces the right access:
#
#   alice  → forge-admins → admin  (admin API 200, maven PUT 201)
#   bob    → forge-devs   → write  (admin API 403, maven PUT 201)
#   carol  → (no mapped group) → fallback read (admin API 403, maven PUT 403)
#   alice with a wrong password → rejected (no session cookie)
#
# Requires: docker, curl, a Go toolchain. Cleans up on exit. Mirrors the way OIDC
# was proven against Keycloak.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
LDAP_NAME="forge-ldap-validate"
LDAP_PORT=13890
FORGE_PORT=18080
FORGE_PID=""
BASE="dc=forge,dc=test"
ADMIN_DN="cn=admin,${BASE}"
ADMIN_PW="adminpw"
PASS=0
FAIL=0

cleanup() {
  [[ -n "$FORGE_PID" ]] && kill "$FORGE_PID" 2>/dev/null || true
  docker rm -f "$LDAP_NAME" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

check() { # check <label> <expected> <actual>
  if [[ "$2" == "$3" ]]; then echo "  PASS: $1 ($3)"; PASS=$((PASS+1));
  else echo "  FAIL: $1 (want $2, got $3)"; FAIL=$((FAIL+1)); fi
}

# ── 1. seed LDIF ──────────────────────────────────────────────────────────────
# osixia/openldap auto-creates the base (dc=forge,dc=test) and cn=admin; the
# bootstrap LDIF below only adds the OUs, users, and groups.
mkdir -p "$WORK/ldifs"
cat > "$WORK/ldifs/seed.ldif" <<LDIF
dn: ou=people,${BASE}
objectClass: organizationalUnit
ou: people

dn: ou=groups,${BASE}
objectClass: organizationalUnit
ou: groups

dn: uid=alice,ou=people,${BASE}
objectClass: inetOrgPerson
cn: Alice Admin
sn: Admin
uid: alice
mail: alice@forge.test
userPassword: alicepw

dn: uid=bob,ou=people,${BASE}
objectClass: inetOrgPerson
cn: Bob Dev
sn: Dev
uid: bob
mail: bob@forge.test
userPassword: bobpw

dn: uid=carol,ou=people,${BASE}
objectClass: inetOrgPerson
cn: Carol Contractor
sn: Contractor
uid: carol
mail: carol@forge.test
userPassword: carolpw

dn: cn=forge-admins,ou=groups,${BASE}
objectClass: groupOfNames
cn: forge-admins
member: uid=alice,ou=people,${BASE}

dn: cn=forge-devs,ou=groups,${BASE}
objectClass: groupOfNames
cn: forge-devs
member: uid=bob,ou=people,${BASE}
LDIF

# ── 2. start OpenLDAP ─────────────────────────────────────────────────────────
echo "== starting OpenLDAP =="
docker rm -f "$LDAP_NAME" >/dev/null 2>&1 || true
docker run -d --name "$LDAP_NAME" \
  -p "${LDAP_PORT}:389" \
  -e LDAP_ORGANISATION="Forge Test" \
  -e LDAP_DOMAIN="forge.test" \
  -e LDAP_ADMIN_PASSWORD="$ADMIN_PW" \
  -v "$WORK/ldifs:/container/service/slapd/assets/config/bootstrap/ldif/custom:ro" \
  osixia/openldap:1.5.0 --copy-service >/dev/null

ldap_ready() {
  docker exec "$LDAP_NAME" ldapsearch -x -H ldap://localhost:389 \
    -D "$ADMIN_DN" -w "$ADMIN_PW" -b "ou=people,${BASE}" "(uid=alice)" uid >/dev/null 2>&1
}
echo -n "  waiting for LDAP"
for _ in $(seq 1 40); do
  if ldap_ready; then echo " ready"; break; fi
  echo -n "."; sleep 1
done
if ! ldap_ready; then
  echo; echo "LDAP failed to seed:"; docker logs "$LDAP_NAME" 2>&1 | tail -30; exit 1
fi

# ── 3. start forge ────────────────────────────────────────────────────────────
echo "== starting forge =="
DATA="$WORK/data"
( cd "$ROOT" && go build -o "$WORK/forge" ./cmd/forge )
"$WORK/forge" -addr ":${FORGE_PORT}" -data "$DATA" -auth \
  -ldap-url "ldap://localhost:${LDAP_PORT}" \
  -ldap-bind-dn "$ADMIN_DN" \
  -ldap-bind-password "$ADMIN_PW" \
  -ldap-user-base-dn "ou=people,${BASE}" \
  -ldap-user-filter '(uid=%s)' \
  -ldap-group-mode search \
  -ldap-group-base-dn "ou=groups,${BASE}" \
  -ldap-group-filter '(member=%s)' \
  -ldap-group-mappings 'forge-admins:admin,forge-devs:write' \
  >"$WORK/forge.log" 2>&1 &
FORGE_PID=$!

echo -n "  waiting for forge"
for _ in $(seq 1 30); do
  if curl -fsS "http://localhost:${FORGE_PORT}/healthz" >/dev/null 2>&1; then echo " ready"; break; fi
  echo -n "."; sleep 1
done

BASEURL="http://localhost:${FORGE_PORT}"

# login <user> <pass> → echoes the forge_token cookie value (empty on failure)
login() {
  local jar="$WORK/jar-$1.txt"; rm -f "$jar"
  curl -fsS -c "$jar" -b "$jar" -o /dev/null \
    --data-urlencode "username=$1" --data-urlencode "password=$2" \
    "${BASEURL}/ui/login" 2>/dev/null || true
  awk '/forge_token/ {print $7}' "$jar" 2>/dev/null || true
}

# admin_status <cookie> → HTTP status hitting an admin-only API
admin_status() { curl -s -o /dev/null -w '%{http_code}' -b "forge_token=$1" "${BASEURL}/api/v1/tokens"; }

# put_status <secret> <path> → HTTP status for a maven PUT (write-gated).
# The login cookie value IS the forge token secret, usable as a Bearer token on
# the /repository/ data plane (which reads Authorization, not the UI cookie).
put_status() {
  curl -s -o /dev/null -w '%{http_code}' -X PUT -H "Authorization: Bearer $1" \
    --data-binary 'hello' "${BASEURL}/repository/maven-hosted/$2"
}

# ── 4. assertions ─────────────────────────────────────────────────────────────
echo "== alice (forge-admins → admin) =="
ATOK="$(login alice alicepw)"
check "session minted"        "yes"  "$([[ -n "$ATOK" ]] && echo yes || echo no)"
check "admin API"             "200"  "$(admin_status "$ATOK")"
check "maven PUT"             "201"  "$(put_status "$ATOK" com/acme/a/1.0/a-1.0.jar)"

echo "== bob (forge-devs → write) =="
BTOK="$(login bob bobpw)"
check "session minted"        "yes"  "$([[ -n "$BTOK" ]] && echo yes || echo no)"
check "admin API forbidden"   "403"  "$(admin_status "$BTOK")"
check "maven PUT"             "201"  "$(put_status "$BTOK" com/acme/b/1.0/b-1.0.jar)"

echo "== carol (no mapped group → fallback read) =="
CTOK="$(login carol carolpw)"
check "session minted"        "yes"  "$([[ -n "$CTOK" ]] && echo yes || echo no)"
check "admin API forbidden"   "403"  "$(admin_status "$CTOK")"
check "maven PUT forbidden"   "403"  "$(put_status "$CTOK" com/acme/c/1.0/c-1.0.jar)"

echo "== alice with wrong password =="
WTOK="$(login alice wrongpw)"
check "no session minted"     "yes"  "$([[ -z "$WTOK" ]] && echo yes || echo no)"

echo
echo "==================== RESULTS: ${PASS} passed, ${FAIL} failed ===================="
[[ "$FAIL" -eq 0 ]]
