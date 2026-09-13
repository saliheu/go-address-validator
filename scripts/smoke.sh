#!/usr/bin/env bash
# End-to-end check against a running stack (docker compose up -d).
# Exercises auth, both validation endpoints, the cache, and refresh-token
# reuse detection.
set -uo pipefail

BASE="${BASE:-http://localhost:8080}"
EMAIL="smoke+$(date +%s)@example.com"
PASS="smoke-password-123"

pass=0; fail=0
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n     got: %s\n' "$1" "$2"; fail=$((fail+1)); }
head() { printf '\n\033[1m%s\033[0m\n' "$1"; }

expect_contains() { # description, haystack, needle
  case "$2" in *"$3"*) ok "$1";; *) bad "$1" "$2";; esac
}

head "health"
r=$(curl -s "$BASE/health")
expect_contains "health reports ok" "$r" '"status":"ok"'

head "auth"
r=$(curl -s -X POST "$BASE/auth/register" -H 'Content-Type: application/json' \
      -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\"}")
expect_contains "register succeeds" "$r" "$EMAIL"

r=$(curl -s -X POST "$BASE/auth/register" -H 'Content-Type: application/json' \
      -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\"}")
expect_contains "duplicate email rejected" "$r" "already registered"

r=$(curl -s -X POST "$BASE/auth/register" -H 'Content-Type: application/json' \
      -d "{\"email\":\"weak@example.com\",\"password\":\"short\"}")
expect_contains "short password rejected" "$r" "error"

r=$(curl -s -X POST "$BASE/auth/login" -H 'Content-Type: application/json' \
      -d "{\"email\":\"$EMAIL\",\"password\":\"wrong-password\"}")
expect_contains "bad password rejected" "$r" "invalid credentials"

r=$(curl -s -X POST "$BASE/auth/login" -H 'Content-Type: application/json' \
      -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\"}")
ACCESS=$(printf '%s' "$r" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
REFRESH=$(printf '%s' "$r" | sed -n 's/.*"refresh_token":"\([^"]*\)".*/\1/p')
[ -n "$ACCESS" ] && ok "login returns an access token" || bad "login returns an access token" "$r"

AUTH=(-H "Authorization: Bearer $ACCESS")

head "authorization"
r=$(curl -s "$BASE/v1/validate-city?country=TR&city=Manisa")
expect_contains "unauthenticated request rejected" "$r" "missing bearer token"

r=$(curl -s -H "Authorization: Bearer not-a-real-token" "$BASE/v1/validate-city?country=TR&city=Manisa")
expect_contains "garbage token rejected" "$r" "invalid or expired token"

head "validate-city"
r=$(curl -s "${AUTH[@]}" "$BASE/v1/validate-city?country=TR&city=Manisa")
expect_contains "exact match is valid" "$r" '"valid":true'

r=$(curl -s "${AUTH[@]}" "$BASE/v1/validate-city?country=TR&city=MANISA")
expect_contains "uppercase input matches" "$r" '"valid":true'

r=$(curl -s "${AUTH[@]}" "$BASE/v1/validate-city?country=TR&city=Sisli")
echo "     (diacritic-free input -> $(printf '%s' "$r" | cut -c1-80))"

r=$(curl -s "${AUTH[@]}" "$BASE/v1/validate-city?country=TR&city=Turgutlu")
expect_contains "district resolves" "$r" '"valid":true'
expect_contains "hierarchy names the parent province" "$r" 'Manisa'
expect_contains "level is labelled in Turkish" "$r" 'ilçe'

r=$(curl -s "${AUTH[@]}" "$BASE/v1/validate-city?country=TR&city=Afyon")
expect_contains "common truncation accepted" "$r" '"valid":true'

r=$(curl -s "${AUTH[@]}" "$BASE/v1/validate-city?country=TR&city=Ankarra")
expect_contains "single typo still matches" "$r" '"valid":true'

r=$(curl -s "${AUTH[@]}" "$BASE/v1/validate-city?country=TR&city=Xyzzyville")
expect_contains "nonsense is rejected" "$r" '"valid":false'

r=$(curl -s "${AUTH[@]}" "$BASE/v1/validate-city?country=TR&city=Merkez&region=Isparta")
expect_contains "region scoping disambiguates Merkez" "$r" '"valid":true'

r=$(curl -s "${AUTH[@]}" "$BASE/v1/validate-city?country=ZZ&city=Nowhere")
expect_contains "unknown country rejected" "$r" "unknown country"

head "variable hierarchy depth"
r=$(curl -s "${AUTH[@]}" "$BASE/v1/countries/TR/levels")
expect_contains "TR level 2 is 'ilçe'" "$r" 'ilçe'
r=$(curl -s "${AUTH[@]}" "$BASE/v1/countries/US/levels")
expect_contains "US level 2 is 'county'" "$r" 'county'
r=$(curl -s "${AUTH[@]}" "$BASE/v1/countries/MC/levels")
expect_contains "MC has a single level" "$r" 'commune'

head "validate-zipcode"
r=$(curl -s "${AUTH[@]}" "$BASE/v1/validate-zipcode?country=TR&code=45400")
expect_contains "known postcode is valid" "$r" '"valid":true'
expect_contains "postcode returns its place" "$r" 'Turgutlu'

r=$(curl -s "${AUTH[@]}" "$BASE/v1/validate-zipcode?country=TR&code=45400&city=Turgutlu")
expect_contains "matching city cross-check passes" "$r" '"city_matches":true'

r=$(curl -s "${AUTH[@]}" "$BASE/v1/validate-zipcode?country=TR&code=45400&city=Ankara")
expect_contains "mismatched city is caught" "$r" '"valid":false'

r=$(curl -s "${AUTH[@]}" "$BASE/v1/validate-zipcode?country=TR&code=99999")
expect_contains "unknown postcode rejected" "$r" '"valid":false'

head "cache"
curl -s -o /dev/null "${AUTH[@]}" "$BASE/v1/validate-city?country=TR&city=Bornova"
h=$(curl -s -D - -o /dev/null "${AUTH[@]}" "$BASE/v1/validate-city?country=TR&city=Bornova" | grep -i '^x-cache')
expect_contains "second identical request hits cache" "$h" "HIT"

head "refresh token rotation"
r=$(curl -s -X POST "$BASE/auth/refresh" -H 'Content-Type: application/json' \
      -d "{\"refresh_token\":\"$REFRESH\"}")
NEW_REFRESH=$(printf '%s' "$r" | sed -n 's/.*"refresh_token":"\([^"]*\)".*/\1/p')
[ -n "$NEW_REFRESH" ] && ok "refresh issues a new token" || bad "refresh issues a new token" "$r"
[ "$NEW_REFRESH" != "$REFRESH" ] && ok "the new token differs from the old one" \
  || bad "the new token differs from the old one" "same value returned"

# Replaying the spent token must burn the whole family, not just fail once.
r=$(curl -s -X POST "$BASE/auth/refresh" -H 'Content-Type: application/json' \
      -d "{\"refresh_token\":\"$REFRESH\"}")
expect_contains "replaying a used token is detected" "$r" "reuse detected"

r=$(curl -s -X POST "$BASE/auth/refresh" -H 'Content-Type: application/json' \
      -d "{\"refresh_token\":\"$NEW_REFRESH\"}")
expect_contains "the successor token is revoked too" "$r" "invalid or expired"

printf '\n\033[1m%d passed, %d failed\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
