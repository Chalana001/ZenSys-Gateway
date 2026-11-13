#!/usr/bin/env bash
# M4 + M5 + M6 + M7 probe: run the real binary and observe the header contract,
# the reverse proxy, the internal lifecycle API, rate limiting, and the size
# limits end to end.
#
# Sections:
#   A  trusted_proxies includes 127.0.0.1 — the header contract is honoured
#   B  trusted_proxies is empty        — nothing is believed
#   C  the reverse proxy: routing, host, headers, streaming
#   E  the body-size limit
#   F  the internal lifecycle API (mutates the registry)
#   G  rate limiting: tier 2, per tenant
#   H  rate limiting: tier 1, per client address, and the health exemption
#   I  rate limiting: a rotated X-Forwarded-For buys nothing
#   J  size limits: an oversized header block
#   D  one product down, the other untouched (§48)
#
# D runs last and that is load-bearing: it stops the POS upstreams to make the
# product-isolation assertion meaningful, so every section that needs a working
# POS frontend must run before it. Sections were reordered once already because
# three assertions were silently measuring a stopped upstream.
#
# Six Git Bash traps are handled explicitly, because all of them fail without
# saying anything useful:
#
#   * curl resolves to the Windows binary, and MSYS rewrites an "-o /dev/null"
#     argument into a path it cannot write, so curl exits 23 regardless of the
#     HTTP status. Every redirect here is done by the shell, and every status
#     probe uses -w instead of discarding the body.
#   * the gateway is a Windows-native binary and cannot read Git Bash's /tmp,
#     so both the configs and the logs live inside the project.
#   * a `kill` on a process that has already exited can hit a reused PID, so
#     every kill is guarded by a liveness check.
#   * `kill` is a hard termination, so a stopped listener can hold its port for
#     a moment. A readiness check alone proves that *something* is listening,
#     not that the process just started is the one answering — see
#     wait_ports_free and start below, which are why they exist.
#
# One further trap is not about Git Bash but about a harness that measures a
# service it also talks to: a liveness probe that is an HTTP request *is* a
# request. On the public listener it is logged and it spends a rate-limit
# token, so a section asserting on the tier-1 bucket is asserting about a
# bucket this script has already spent from — which is precisely how the
# "a rotated X-Forwarded-For buys nothing" section first failed, with a 429
# where it wanted a 200. wait_ports_free therefore asks the kernel with a raw
# TCP connect (port_listening) rather than asking the service.
#
# Preconditions:
#   * ZENSYS_REGISTRY_DSN names a registry holding two ACTIVE tenants —
#     'mangala' (POS) and 'garage1' (GMS). The script says so and stops if
#     either is missing rather than reporting a misleading failure.
#   * the four upstream ports in configs/gateway.dev.yaml are free.
#
# Section F mutates the registry: it reserves, activates, disables, and releases
# a tenant of its own ('probe-lc-1'), and releases it again at the start of the
# next run if a previous one was interrupted. It never mutates the two fixtures.
#
# Note that the MySQL integration tests (internal/tenant, gated on
# ZENSYS_TEST_DSN) DELETE FROM tenant_routes as part of their setup, so running
# them against the same database empties this harness's fixtures. The
# precondition check below reports that rather than failing somewhere
# confusing.

set -u

cd "$(dirname "$0")/../.." || exit 1

if [ -z "${ZENSYS_REGISTRY_DSN:-}" ]; then
  echo "FAIL: ZENSYS_REGISTRY_DSN is not set."
  echo "      The probe needs a registry holding two ACTIVE tenants:"
  echo "        mangala (POS) and garage1 (GMS)."
  exit 1
fi

# The lifecycle section authenticates with these. They must match what the
# running process resolved from the same variables, and config.Load rejects
# anything shorter than 32 characters.
export ZENSYS_TOKEN_POS="${ZENSYS_TOKEN_POS:-$(printf 'a%.0s' $(seq 1 32))}"
export ZENSYS_TOKEN_GMS="${ZENSYS_TOKEN_GMS:-$(printf 'b%.0s' $(seq 1 32))}"

# Go is not on the sandboxed shell's PATH by default.
if ! command -v go >/dev/null 2>&1; then
  export PATH="/c/Program Files/Go/bin:$PATH"
fi
if ! command -v go >/dev/null 2>&1; then
  echo "FAIL: go is not on PATH; the probe builds its upstream stand-in."
  exit 1
fi

PUBLIC=http://127.0.0.1:18080
INTERNAL=http://127.0.0.1:18081

# The host and port alone, for the socket checks that must not send a request.
# Derived from the URLs above so there is one place to change an address.
PUBLIC_ADDR="${PUBLIC#http://}"
INTERNAL_ADDR="${INTERNAL#http://}"

BASE=configs/gateway.dev.yaml
A=configs/.probe/a-trusting.yaml
B=configs/.probe/b-distrusting.yaml
C=configs/.probe/c-smallbody.yaml
F=configs/.probe/f-longttl.yaml
G=configs/.probe/g-tinytiers.yaml
H=configs/.probe/h-perclient.yaml
I=configs/.probe/i-spoofproof.yaml
K=configs/.probe/k-headerlimit.yaml
UP_LOG=configs/.probe/upstreams.log
UPSTREAM_BIN=bin/probe-upstream.exe
GATEWAY_BIN=bin/gateway.exe

# Section F creates and destroys its own tenant. The slug is fixed so that an
# interrupted run can clean up after itself on the next one.
LIFECYCLE_SLUG=probe-lc-1

# A TTL long enough that a passing assertion can only mean invalidation worked.
# With the dev config's 60s this would be ambiguous: the route would become
# visible on its own, and the check would pass whether or not the lifecycle API
# invalidated anything.
LONG_TTL=300s

# A body limit small enough to exceed without generating a 10 MB request. The
# probe asserts the substitution, so a renamed key cannot silently leave this
# config at the shipped 10 MB and turn the oversize check into a request that
# succeeds for the wrong reason.
SMALL_BODY_LIMIT=1024

# The rate-limit sections need buckets that cannot refill while they run.
#
# burst is exact — a bucket starts full, so "the third request is refused" is
# only a statement about the burst if no token was earned in between. At rps 1
# a slow section would earn tokens and the assertion would pass or fail
# depending on how fast the machine is. At 0.01 a token takes 100 seconds, so
# nothing refills, and the Retry-After the Gateway reports is ~100 rather than
# a number the test has to race.
NO_REFILL_RPS=0.01

# Tier-1 burst for section G. Deliberately large: G is about tier 2, and a
# tier-1 bucket small enough to exhaust would make a 429 there ambiguous
# between the two tiers. Section H is where tier 1 is made small.
#
# 200 rather than a rounder number only because the shipped values are 400 and
# 100, and the stale-substitution check below greps for those as fixed strings
# — a burst of 100 would be indistinguishable from a substitution that never
# applied.
TIER1_BURST_G=200

# Tier-2 burst for section G: two requests per tenant, so the third is refused
# by tier 2 while tier 1 still has tokens.
TIER2_BURST_G=2

# Tier-2 burst for sections H and I, which are about tier 1. Large enough that
# tier 2 cannot refuse anything, so a 429 in those sections can only have come
# from tier 1. Not 100000, because "burst: 100" is one of the shipped values
# the stale check greps for and it is a substring of it.
TIER2_BURST_HI=99999

# Tier-1 burst for sections H and I: two requests per key.
TIER1_BURST_HI=2

# A header limit small enough to exceed with a normal-looking request, and
# large enough that the harness's own requests fit inside it. curl sends
# Host, User-Agent, and Accept, which is well under 200 bytes.
SMALL_HEADER_LIMIT=1024

# The size of the oversized header the probe sends. Comfortably over the limit
# and comfortably under net/http's hard cap of max_header_bytes + 4096, so the
# request is refused by the Gateway's own check rather than by net/http — which
# is the whole point of the assertion.
OVERSIZED_HEADER=2048

# The upstream ports come from the config the project actually ships. They are
# read out of it rather than repeated here so that a change to the dev config
# cannot leave the probe pointing at a socket nothing is listening on.
POS_FE_PORT=$(sed -n '/^  POS:/,/^  GMS:/p' "$BASE" | sed -n 's|.*frontend_upstream: "http://127\.0\.0\.1:\([0-9]*\)".*|\1|p')
POS_BE_PORT=$(sed -n '/^  POS:/,/^  GMS:/p' "$BASE" | sed -n 's|.*backend_upstream: "http://127\.0\.0\.1:\([0-9]*\)".*|\1|p')
GMS_FE_PORT=$(sed -n '/^  GMS:/,$p' "$BASE" | sed -n 's|.*frontend_upstream: "http://127\.0\.0\.1:\([0-9]*\)".*|\1|p')
GMS_BE_PORT=$(sed -n '/^  GMS:/,$p' "$BASE" | sed -n 's|.*backend_upstream: "http://127\.0\.0\.1:\([0-9]*\)".*|\1|p')

# The probe configs are derived from the config the project actually ships, so
# they cannot drift from it: a key added to the real config appears here too.
# Generating them in run.sh rather than expecting them to exist means the
# harness is self-contained — a probe that depends on files nobody produces
# fails at the worst moment.
#
# Each substitution is asserted. A renamed key would otherwise turn the probe
# into a silent copy of the dev config, and the run would verify the wrong
# thing while reporting success.
generate_configs() {
  sed -e 's|127\.0\.0\.1:8080|127.0.0.1:18080|' \
      -e 's|127\.0\.0\.1:8081|127.0.0.1:18081|' \
      -e 's|log_format: "text"|log_format: "json"|' \
      -e 's|^  route_ttl: 60s|  route_ttl: 2s|' \
      -e 's|^  negative_ttl: 30s|  negative_ttl: 2s|' \
      "$BASE" >"$A"

  # The only difference between the two runs: whether anything is trusted.
  sed -e 's|^  trusted_proxies:$|  trusted_proxies: []|' \
      -e '/^    - "127\.0\.0\.1\/32"$/d' \
      -e '/^    - "::1\/128"$/d' \
      "$A" >"$B"

  # A trust set that is unchanged, but a body limit the probe can reach.
  sed -e "s|^  max_body_bytes: 10485760|  max_body_bytes: $SMALL_BODY_LIMIT|" "$A" >"$C"

  # The same as A except for the TTLs, which are long enough that nothing can
  # converge on its own during the run. Section F depends on this: a short TTL
  # would let the cache expire between the mutation and the check, so the check
  # would pass whether or not the mutation invalidated anything.
  #
  # Derived from $BASE rather than from $A, so it does not depend on $A having
  # rewritten the TTL first. A chain of substitutions would silently stop
  # matching the moment an earlier one changed.
  sed -e 's|127\.0\.0\.1:8080|127.0.0.1:18080|' \
      -e 's|127\.0\.0\.1:8081|127.0.0.1:18081|' \
      -e 's|log_format: "text"|log_format: "json"|' \
      -e 's|^  route_ttl: 60s|  route_ttl: '"$LONG_TTL"'|' \
      -e 's|^  negative_ttl: 30s|  negative_ttl: '"$LONG_TTL"'|' \
      "$BASE" >"$F"

  # Section G: tier 2 made small, tier 1 left large.
  #
  # Tier 1 is deliberately generous here so that a 429 in this section cannot
  # have come from it. The operational-path exemption needs a *small* tier-1
  # bucket to be observable, so it is asserted in section H instead, against a
  # listener that serves health on the public address.
  sed -e 's|127\.0\.0\.1:8080|127.0.0.1:18080|' \
      -e 's|127\.0\.0\.1:8081|127.0.0.1:18081|' \
      -e 's|log_format: "text"|log_format: "json"|' \
      -e 's|^    rps: 200$|    rps: '"$NO_REFILL_RPS"'|' \
      -e 's|^    burst: 400$|    burst: '"$TIER1_BURST_G"'|' \
      -e 's|^    rps: 50$|    rps: '"$NO_REFILL_RPS"'|' \
      -e 's|^    burst: 100$|    burst: '"$TIER2_BURST_G"'|' \
      "$BASE" >"$G"

  # Section J: only the header limit changes. The rate-limit buckets are left
  # at their shipped, generous values on purpose — section J asserts that an
  # oversized header is refused while an ordinary request is served, and a
  # small tier bucket here would make the second half of that a 429 and hide
  # whether the 431 had anything to do with the header.
  sed -e 's|127\.0\.0\.1:8080|127.0.0.1:18080|' \
      -e 's|127\.0\.0\.1:8081|127.0.0.1:18081|' \
      -e 's|log_format: "text"|log_format: "json"|' \
      -e 's|^  max_header_bytes: 65536|  max_header_bytes: '"$SMALL_HEADER_LIMIT"'|' \
      "$BASE" >"$K"

  # Section H: tier 1 only, and small enough that per-client keying is
  # observable. Tier 2 is left enormous so a refusal here can only have come
  # from tier 1. health_on_public is on so the operational-path exemption is
  # observable against a bucket small enough for it to matter.
  sed -e 's|127\.0\.0\.1:8080|127.0.0.1:18080|' \
      -e 's|127\.0\.0\.1:8081|127.0.0.1:18081|' \
      -e 's|log_format: "text"|log_format: "json"|' \
      -e 's|^    rps: 200$|    rps: '"$NO_REFILL_RPS"'|' \
      -e 's|^    burst: 400$|    burst: '"$TIER1_BURST_HI"'|' \
      -e 's|^    rps: 50$|    rps: '"$NO_REFILL_RPS"'|' \
      -e 's|^    burst: 100$|    burst: '"$TIER2_BURST_HI"'|' \
      -e 's|^  health_on_public: false|  health_on_public: true|' \
      "$BASE" >"$H"

  # Section I: the same tier-1 bucket, but nothing is trusted, so every request
  # is attributed to the socket address. Derived from $B, which already has an
  # empty trusted_proxies, so the only further change is the rate limit.
  sed -e 's|^    rps: 200$|    rps: '"$NO_REFILL_RPS"'|' \
      -e 's|^    burst: 400$|    burst: '"$TIER1_BURST_HI"'|' \
      -e 's|^    rps: 50$|    rps: '"$NO_REFILL_RPS"'|' \
      -e 's|^    burst: 100$|    burst: '"$TIER2_BURST_HI"'|' \
      "$B" >"$I"

  local missing=0
  for pair in "$A:127.0.0.1:18080" "$A:route_ttl: 2s" "$A:log_format: \"json\"" \
              "$B:trusted_proxies: []" "$C:max_body_bytes: $SMALL_BODY_LIMIT" \
              "$F:route_ttl: $LONG_TTL" "$F:negative_ttl: $LONG_TTL" \
              "$G:burst: $TIER1_BURST_G" "$G:burst: $TIER2_BURST_G" \
              "$H:burst: $TIER1_BURST_HI" "$H:burst: $TIER2_BURST_HI" \
              "$H:health_on_public: true" \
              "$I:burst: $TIER1_BURST_HI" "$I:burst: $TIER2_BURST_HI" \
              "$I:trusted_proxies: []" \
              "$K:max_header_bytes: $SMALL_HEADER_LIMIT"; do
    file="${pair%%:*}"; want="${pair#*:}"
    if ! grep -qF "$want" "$file"; then
      echo "FAIL: $file does not contain '$want'; the probe config has drifted from $BASE"
      missing=1
    fi
  done

  # A tier that still carried a shipped value would make a section's assertion
  # measure nothing: with burst 400 the "third request is refused" check would
  # be a request that succeeded, and the failure would look like a limiter that
  # does not work rather than a config that never applied.
  for pair in "$G:burst: 400" "$G:burst: 100" "$G:rps: 200" \
              "$H:burst: 400" "$H:burst: 100" \
              "$I:burst: 400" "$I:burst: 100" \
              "$K:max_header_bytes: 65536"; do
    file="${pair%%:*}"; stale="${pair#*:}"
    if grep -qF "$stale" "$file"; then
      echo "FAIL: $file still carries '$stale'; the substitution did not apply"
      missing=1
    fi
  done

  # The internal listener must stay on loopback. Section F asserts the lifecycle
  # API is not reachable from the public address, and that assertion is only
  # meaningful while the private listener is actually private.
  if ! grep -qE '^  internal_addr: "127\.0\.0\.1:' "$F"; then
    echo "FAIL: $F does not bind internal_addr to loopback; the private listener would not be private"
    missing=1
  fi

  # A config that still carried the shipped limit would make the oversize
  # assertion below pass while testing nothing.
  if grep -qF 'max_body_bytes: 10485760' "$C"; then
    echo "FAIL: $C still carries the shipped max_body_bytes; the substitution did not apply"
    missing=1
  fi

  # A port that could not be read would silently start an upstream on the wrong
  # socket, and every routing assertion below would then be measuring nothing.
  for pair in "POS_FE_PORT:$POS_FE_PORT" "POS_BE_PORT:$POS_BE_PORT" \
              "GMS_FE_PORT:$GMS_FE_PORT" "GMS_BE_PORT:$GMS_BE_PORT"; do
    name="${pair%%:*}"; value="${pair#*:}"
    if [ -z "$value" ]; then
      echo "FAIL: could not read $name from $BASE; the probe's upstreams would listen on the wrong ports"
      missing=1
    fi
  done

  [ "$missing" -eq 0 ] || exit 1
}

generate_configs

# --- upstream stand-ins ---------------------------------------------------

UPSTREAM_PIDS=""

start_upstream() {
  local name="$1" addr="$2"
  "$UPSTREAM_BIN" -name "$name" -addr "$addr" >>"$UP_LOG" 2>&1 &
  echo $!
}

wait_for_upstream() {
  local addr="$1"
  for _ in $(seq 1 40); do
    if curl -sf "http://$addr/__health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  echo "FAIL: no upstream answered on $addr"
  cat "$UP_LOG"
  return 1
}

start_all_upstreams() {
  : >"$UP_LOG"

  POS_FE_PID=$(start_upstream pos-frontend "127.0.0.1:$POS_FE_PORT")
  POS_BE_PID=$(start_upstream pos-backend "127.0.0.1:$POS_BE_PORT")
  GMS_FE_PID=$(start_upstream gms-frontend "127.0.0.1:$GMS_FE_PORT")
  GMS_BE_PID=$(start_upstream gms-backend "127.0.0.1:$GMS_BE_PORT")
  UPSTREAM_PIDS="$POS_FE_PID $POS_BE_PID $GMS_FE_PID $GMS_BE_PID"

  wait_for_upstream "127.0.0.1:$POS_FE_PORT" || exit 1
  wait_for_upstream "127.0.0.1:$POS_BE_PORT" || exit 1
  wait_for_upstream "127.0.0.1:$GMS_FE_PORT" || exit 1
  wait_for_upstream "127.0.0.1:$GMS_BE_PORT" || exit 1
}

kill_pid() {
  local pid="${1:-}"
  [ -n "$pid" ] || return 0
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null
    wait "$pid" 2>/dev/null
  fi
}

stop_upstreams() {
  for pid in $UPSTREAM_PIDS; do
    kill_pid "$pid"
  done
  UPSTREAM_PIDS=""
}

# stop_pos_upstreams leaves the GMS pair running, which is what makes the
# product-isolation assertion in section D meaningful: one process, two
# products, only one of them broken.
stop_pos_upstreams() {
  kill_pid "${POS_FE_PID:-}"
  kill_pid "${POS_BE_PID:-}"
}

# --- the gateway ----------------------------------------------------------

# port_listening HOST PORT -> true when something accepts a TCP connection.
#
# A raw connect rather than an HTTP request, because this asks whether the
# *socket* has been released, and an HTTP probe answers a different question
# with side effects. On the public listener a probe is a request like any
# other: it is logged, and it spends a tier-1 rate-limit token. That is not
# hypothetical. A section measuring the tier-1 bucket found one of its two
# tokens already spent by this function, so "a rotated X-Forwarded-For buys
# nothing" failed with a 429 where it wanted a 200 — the harness had quietly
# become a client. The fix is not a larger burst; it is to stop issuing
# requests in order to ask about sockets.
#
# /dev/tcp is a bash built-in, so this costs no process either, where each
# curl invocation above costs one.
port_listening() {
  (exec 3<>"/dev/tcp/$1/$2") >/dev/null 2>&1
}

# wait_ports_free polls until nothing accepts a connection on either port,
# which is the signal that the previous gateway has actually released its
# sockets.
#
# This is not belt-and-braces. Without it, a gateway that has been signalled
# but has not finished draining lets the next one start on top of it: the new
# process dies on a bind error, the old one keeps answering, and every probe in
# the section silently measures the wrong process with the wrong config. That
# is exactly how a body-limit check once reported a limit that was never
# applied.
wait_ports_free() {
  for _ in $(seq 1 80); do
    # Both ports, because a gateway part-way through shutdown can stop
    # answering one while still holding the other, and it is the public port
    # the probes actually use.
    if ! port_listening "${INTERNAL_ADDR%:*}" "${INTERNAL_ADDR##*:}" &&
       ! port_listening "${PUBLIC_ADDR%:*}" "${PUBLIC_ADDR##*:}"; then
      return 0
    fi
    sleep 0.25
  done

  echo "FAIL: something is still listening after 20s; the next section would measure it"
  echo "      internal $INTERNAL_ADDR  -> $(port_listening "${INTERNAL_ADDR%:*}" "${INTERNAL_ADDR##*:}" && echo listening || echo free)"
  echo "      public   $PUBLIC_ADDR    -> $(port_listening "${PUBLIC_ADDR%:*}" "${PUBLIC_ADDR##*:}" && echo listening || echo free)"
  echo "      gateway processes  -> $(tasklist 2>/dev/null | grep -ci gateway)"
  return 1
}

start() {
  local config="$1" log="$2"

  wait_ports_free || return 1

  : >"$log"
  "$GATEWAY_BIN" -config "$config" >"$log" 2>&1 &
  GW_PID=$!

  for _ in $(seq 1 60); do
    # Liveness first. Readiness alone does not prove that *this* process is the
    # one serving, because a stale gateway answers the same endpoint.
    if ! kill -0 "$GW_PID" 2>/dev/null; then
      echo "FAIL: the gateway exited during startup:"
      cat "$log"
      return 1
    fi
    if curl -sf "$INTERNAL/readyz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.25
  done

  echo "FAIL: the gateway never became ready"
  cat "$log"
  return 1
}

stop() {
  kill_pid "${GW_PID:-}"
  wait_ports_free || exit 1
}

# --- assertions -----------------------------------------------------------

FAILURES=0

check() {
  local label="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then
    printf '  ok   %-52s %s\n' "$label" "$got"
  else
    printf '  FAIL %-52s got %s, want %s\n' "$label" "$got" "$want"
    FAILURES=$((FAILURES + 1))
  fi
}

check_contains() {
  local label="$1" needle="$2" haystack="$3"
  case "$haystack" in
    *"$needle"*)
      printf '  ok   %-52s %s\n' "$label" "$needle" ;;
    *)
      printf '  FAIL %-52s %q does not contain %q\n' "$label" "$haystack" "$needle"
      FAILURES=$((FAILURES + 1)) ;;
  esac
}

# body HOST PATH -> the response body.
body() {
  curl -s -H "Host: $1" "$PUBLIC$2"
}

# status HOST PATH -> the status code.
#
# It uses -w rather than "-o /dev/null" on purpose: the shell cannot redirect
# curl's own output, and the Windows curl exits 23 whenever it is asked to
# write to a path MSYS has rewritten. -w keeps the code on stdout where the
# pipeline can read it.
status() {
  curl -s -H "Host: $1" -w '\n%{http_code}' "$PUBLIC$2" | tail -1
}

# post_status HOST PATH FILE -> the status code for a POST of FILE.
post_status() {
  curl -s -X POST -H "Host: $1" --data-binary @"$3" -w '\n%{http_code}' "$PUBLIC$2" | tail -1
}

# post_body HOST PATH FILE -> the response body for a POST of FILE.
post_body() {
  curl -s -X POST -H "Host: $1" --data-binary @"$3" "$PUBLIC$2"
}

# json_field JSON KEY -> the first string value stored under KEY.
json_field() {
  printf '%s' "$1" | grep -o "\"$2\":\"[^\"]*\"" | head -1 | sed 's/^[^:]*:"\(.*\)"$/\1/'
}

# --- rate-limit helpers ---------------------------------------------------
#
# The rate-limit sections need to send extra request headers — a forwarded
# client address, an oversized header — and to read a response header. The
# helpers above take only a Host, so these are the same shapes with the header
# list appended.

# status_h HOST PATH [HEADER...] -> the status code.
status_h() {
  local host="$1" path="$2"
  shift 2

  local args=(-s -H "Host: $host" -w '\n%{http_code}')
  while [ $# -gt 0 ]; do
    args+=(-H "$1")
    shift
  done

  curl "${args[@]}" "$PUBLIC$path" | tail -1
}

# body_h HOST PATH [HEADER...] -> the response body.
body_h() {
  local host="$1" path="$2"
  shift 2

  local args=(-s -H "Host: $host")
  while [ $# -gt 0 ]; do
    args+=(-H "$1")
    shift
  done

  curl "${args[@]}" "$PUBLIC$path"
}

# resp_header HOST PATH NAME [HEADER...] -> the named response header.
#
# -i rather than -D so the headers and the body arrive on one stream and the
# single request that produces them is the request being asserted about. A
# second request would be a second rate-limit decision.
resp_header() {
  local host="$1" path="$2" name="$3"
  shift 3

  local args=(-s -i -H "Host: $host")
  while [ $# -gt 0 ]; do
    args+=(-H "$1")
    shift
  done

  curl "${args[@]}" "$PUBLIC$path" | grep -i "^$name:" | head -1 | tr -d '\r' | sed 's/^[^:]*: *//'
}

# repeat_char CHAR COUNT -> COUNT copies of CHAR, for building large headers.
#
# printf with a field width rather than a loop: seq plus a loop is a subshell
# per iteration, and this is called with counts in the thousands.
repeat_char() {
  printf "%${2}s" "" | tr ' ' "$1"
}

# --- internal lifecycle API helpers ---------------------------------------
#
# The lifecycle API needs both the status and the body of the *same* response:
# a second request would be a duplicate and answer 409, so the two cannot be
# fetched separately. icall therefore sets both in globals, and the thin
# wrappers read them.

ICALL_STATUS=""
ICALL_BODY=""

# icall METHOD PATH TOKEN [JSON]
#
# An empty TOKEN sends no Authorization header, which is how the
# unauthenticated case is expressed. An empty JSON sends no body.
icall() {
  local method="$1" path="$2" token="$3" data="${4:-}"
  local args=(-s -X "$method" -w '\n%{http_code}')

  if [ -n "$token" ]; then
    args+=(-H "Authorization: Bearer $token")
  fi
  if [ -n "$data" ]; then
    args+=(-H 'Content-Type: application/json' --data-binary "$data")
  fi

  local out
  out="$(curl "${args[@]}" "$INTERNAL$path")"

  ICALL_STATUS="$(printf '%s' "$out" | tail -1)"
  ICALL_BODY="$(printf '%s' "$out" | sed '$d')"
}

# icode METHOD PATH TOKEN [JSON] -> the status code.
icode() {
  icall "$@"
  printf '%s' "$ICALL_STATUS"
}

# istatus PATH [CURL-ARG...] -> the status code, for requests whose headers the
# helpers above cannot express (a wrong scheme, two Authorization headers).
istatus() {
  local path="$1"
  shift
  ICALL_STATUS="$(curl -s "$@" -w '\n%{http_code}' "$INTERNAL$path" | tail -1)"
  printf '%s' "$ICALL_STATUS"
}

# probe LABEL HOST [HEADER...] prints the status line and the request ID.
probe() {
  local label="$1" host="$2"
  shift 2

  local args=(-s -i -H "Host: $host")
  while [ $# -gt 0 ]; do
    args+=(-H "$1")
    shift
  done

  local out status_line rid
  out="$(curl "${args[@]}" "$PUBLIC/")"
  status_line="$(printf '%s' "$out" | head -1 | tr -d '\r')"
  rid="$(printf '%s' "$out" | grep -i '^x-request-id:' | tr -d '\r' | awk '{print $2}')"

  printf '%-46s %s\n' "$label" "$status_line"
  printf '%-46s X-Request-ID=%s\n' "" "${rid:-<absent>}"
}

# require_tenant HOST SLUG PRODUCT fails loudly with the SQL to fix it.
require_tenant() {
  local host="$1" slug="$2" product="$3"
  local code
  code="$(status "$host" "/__echo")"
  if [ "$code" = "404" ]; then
    echo "FAIL: $slug is not an ACTIVE tenant, so the probe cannot test routing."
    echo "      Insert it with:"
    echo "        INSERT INTO tenant_routes (tenant_slug, product, status)"
    echo "        VALUES ('$slug', '$product', 'ACTIVE');"
    exit 1
  fi
}

# --- build ----------------------------------------------------------------

echo "Building the probe's upstream stand-in..."
if ! go build -o "$UPSTREAM_BIN" ./configs/.probe/upstream; then
  echo "FAIL: could not build $UPSTREAM_BIN"
  exit 1
fi

# The gateway is built here rather than assumed, for the same reason the
# upstream stand-in is: a harness that measures a binary it did not build can
# measure a binary that is not the source it is testing.
#
# This is not hypothetical. The M7 run reported a tier-2 refusal with no tenant
# in its log line after the fix for exactly that was committed, because
# bin/gateway.exe was 20 minutes older than internal/httpapi/public.go. The
# check that existed before this one asserted only that the file was *present*,
# which is the weakest possible form of the check and reads as coverage — the
# same shape as M0's 405 test that passed because it omitted the production
# catch-all, and M6's probe config whose substitution had silently stopped
# matching. Existence is not freshness.
echo "Building the gateway under test..."
if ! go build -o "$GATEWAY_BIN" ./cmd/gateway; then
  echo "FAIL: could not build $GATEWAY_BIN"
  exit 1
fi

# Belt and braces: the build above is authoritative, but a stale binary that
# somehow survives it (a failed build leaving the old file, a source file with
# a future timestamp) must not be measured silently.
newer="$(find . -name '*.go' -newer "$GATEWAY_BIN" -not -path './configs/.probe/*' -print -quit 2>/dev/null)"
if [ -n "$newer" ]; then
  echo "FAIL: $GATEWAY_BIN is older than $newer, so it is not the code under test."
  echo "      This should be impossible after the build above; investigate before trusting a run."
  exit 1
fi

echo "Starting the four upstream stand-ins..."
start_all_upstreams

cleanup() {
  stop
  stop_upstreams
}
trap cleanup EXIT

echo
echo "=============================================================="
echo "A. trusted_proxies includes 127.0.0.1 (loopback is the proxy)"
echo "=============================================================="

start "$A" configs/.probe/a.log || exit 1

require_tenant mangala.zensys.lk mangala POS
require_tenant garage1.zensys.lk garage1 GMS

probe "mangala.zensys.lk, inbound request id" "mangala.zensys.lk" \
  "X-Request-ID: trace-abc-123"

probe "mangala.zensys.lk, spoofed tenant header" "mangala.zensys.lk" \
  "X-Tenant-ID: nonexistent-tenant"

probe "garage1.zensys.lk, trusted forwarded headers" "garage1.zensys.lk" \
  "X-Forwarded-Proto: https" "X-Forwarded-Host: mangala.zensys.lk"

probe "unknown.zensys.lk" "unknown.zensys.lk"

probe "reserved host (www)" "www.zensys.lk"

echo
echo "--- access log: resolved requests ---"
grep '"msg":"request"' configs/.probe/a.log | grep '"tenant"' || echo "(none)"

echo
echo "--- access log: rejected requests ---"
grep '"msg":"request"' configs/.probe/a.log | grep -v '"tenant"' || echo "(none)"

echo
echo "--- host_label is not confused with tenant ---"
grep -c '"host_label"' configs/.probe/a.log

echo
echo "--- upstreams the process was configured with ---"
grep -o '"routes":\[[^]]*\]' configs/.probe/a.log | head -1 || echo "(no route list logged)"

stop

echo
echo "=============================================================="
echo "B. trusted_proxies is empty (nothing is trusted)"
echo "=============================================================="

start "$B" configs/.probe/b.log || exit 1

probe "mangala.zensys.lk, inbound request id" "mangala.zensys.lk" \
  "X-Request-ID: trace-abc-123"

# The security-relevant case. If an untrusted X-Forwarded-Host or X-Tenant-ID
# influenced routing, one of these would resolve to nothing and answer 404; a
# 200 from the POS frontend means the host the request actually arrived on is
# still what decides.
probe "mangala.zensys.lk, spoofed headers ignored" "mangala.zensys.lk" \
  "X-Forwarded-Host: evil.example.com" "X-Tenant-ID: nonexistent-tenant"

probe "mangala.zensys.lk, spoofed host is still 404" "evil.example.com"

echo
echo "-- the spoofed tenant header still does not reach the upstream --"
check "spoofed tenant header is not forwarded" "mangala" \
  "$(json_field "$(body mangala.zensys.lk /__echo)" "X-Tenant-ID")"

stop

echo
echo "--- startup warning about an empty trust list ---"
grep -i 'trusted proxies' configs/.probe/b.log || echo "(no warning was logged)"

echo "=============================================================="
echo "C. the reverse proxy: routing, host, headers, streaming"
echo "=============================================================="

start "$A" configs/.probe/c.log || exit 1

echo
echo "-- the four §90 routing rows --"
check_contains "POS tenant at /"            "upstream=pos-frontend" "$(body mangala.zensys.lk /)"
check_contains "POS tenant under /api"      "upstream=pos-backend"  "$(body mangala.zensys.lk /api/orders)"
check_contains "GMS tenant at /"            "upstream=gms-frontend" "$(body garage1.zensys.lk /)"
check_contains "GMS tenant under /api/v1"   "upstream=gms-backend"  "$(body garage1.zensys.lk /api/v1/tenants)"

echo
echo "-- the path reaches the product unchanged (§21) --"
check_contains "path is not rewritten" "/api/orders" "$(body mangala.zensys.lk /api/orders)"
check_contains "query is preserved"    "limit=10"    "$(body mangala.zensys.lk '/api/orders?limit=10')"

echo
echo "-- the header contract, as the upstream saw it (§25, §44) --"

# echo_field LABEL EXPECTED HOST PATH KEY [HEADER...]
#
# The expected value comes first so that the call sites read as assertions
# rather than as a list of arguments whose meaning has to be looked up.
echo_field() {
  local label="$1" want="$2" host="$3" path="$4" key="$5"
  shift 5

  local args=(-s -H "Host: $host")
  while [ $# -gt 0 ]; do
    args+=(-H "$1")
    shift
  done

  local json
  json="$(curl "${args[@]}" "$PUBLIC$path")"
  check "$label" "$want" "$(json_field "$json" "$key")"
}

# The product must be handed the public hostname. GMS resolves its own tenant
# from this header, so an internal address here would break tenant resolution
# inside the product rather than merely look wrong in a log.
echo_field "upstream saw Host = the public hostname" \
  "garage1.zensys.lk" "garage1.zensys.lk" "/__echo" "host"

echo_field "upstream saw the canonical tenant" \
  "mangala" "mangala.zensys.lk" "/__echo" "X-Tenant-ID"

echo_field "a spoofed tenant header never arrives" \
  "mangala" "mangala.zensys.lk" "/__echo" "X-Tenant-ID" "X-Tenant-ID: spoofed"

echo_field "X-Forwarded-For is the resolved client" \
  "127.0.0.1" "mangala.zensys.lk" "/__echo" "X-Forwarded-For"

# The Gateway's own connection is plain HTTP; the browser's was HTTPS. The
# product must be told what the client used, not what the Gateway saw.
echo_field "a trusted X-Forwarded-Proto survives to the product" \
  "https" "mangala.zensys.lk" "/__echo" "X-Forwarded-Proto" "X-Forwarded-Proto: https"

echo
echo "-- streaming is not buffered --"

# curl measures the time to the first response byte itself, which is the
# measurement this needs: piping through head would leave curl blocked until
# the upstream sent its second event, and the shell would then time the
# upstream's 2s gap rather than the flush.
#
# --max-time cuts the transfer off after the first event, and -w still prints
# on that path. The body goes to a file inside the project because the Windows
# curl cannot write to /dev/null.
STREAM_OUT=configs/.probe/stream.out
TTFB="$(curl -sN --max-time 1 -o "$STREAM_OUT" -w '%{time_starttransfer}' \
  -H "Host: mangala.zensys.lk" "$PUBLIC/__stream" 2>/dev/null || true)"

if awk -v t="$TTFB" 'BEGIN { exit !(t > 0 && t < 1.5) }'; then
  printf '  ok   %-52s %ss (upstream holds for 2s)\n' "the first event arrived before the gap" "$TTFB"
else
  printf '  FAIL %-52s time_starttransfer=%s; the response was buffered somewhere\n' \
    "the first event arrived before the gap" "${TTFB:-<none>}"
  FAILURES=$((FAILURES + 1))
fi
check_contains "the streamed body carries the first event" "event: first" "$(cat "$STREAM_OUT" 2>/dev/null || true)"

stop

echo
echo "=============================================================="
echo "E. request bodies"
echo "=============================================================="

# Until M5 nothing read a request body on the public listener — the terminal
# step answered 503 without looking at one — so this is the first milestone at
# which the body path, and the body limit, can be observed at all.
start "$C" configs/.probe/e.log || exit 1

SMALL=configs/.probe/small.body
BIG=configs/.probe/big.body
printf 'hello-from-the-probe' >"$SMALL"
head -c $((SMALL_BODY_LIMIT * 4)) /dev/zero | tr '\0' 'a' >"$BIG"

echo
echo "-- a body within the limit reaches the product intact --"
SMALL_SHA=$(sha256sum "$SMALL" | cut -c1-16)
SMALL_OUT="$(post_body mangala.zensys.lk /submit "$SMALL")"
check_contains "the body length arrived intact" "body_bytes=20" "$SMALL_OUT"
check_contains "the body content arrived intact" "body_sha256=$SMALL_SHA" "$SMALL_OUT"

echo
echo "-- a body over max_body_bytes ($SMALL_BODY_LIMIT bytes) --"
check "status for a body over the limit" "413" "$(post_status mangala.zensys.lk /submit "$BIG")"

BIG_OUT="$(post_body mangala.zensys.lk /submit "$BIG")"
check_contains "the error names the size, not the upstream" \
  '"error":"payload_too_large"' "$BIG_OUT"

# The Gateway answered this itself. A 413 that came back carrying a product's
# identity would mean the oversized body had been forwarded after all.
if printf '%s' "$BIG_OUT" | grep -q 'upstream='; then
  printf '  FAIL %-52s %s\n' "the oversized request was answered by the Gateway" "$BIG_OUT"
  FAILURES=$((FAILURES + 1))
else
  printf '  ok   %-52s %s\n' "the oversized request was answered by the Gateway" "$BIG_OUT"
fi

stop

echo
echo "=============================================================="
echo "F. internal lifecycle API (§39-§42, §80)"
echo "=============================================================="

start "$F" configs/.probe/f.log || exit 1

require_tenant mangala.zensys.lk mangala POS
require_tenant garage1.zensys.lk garage1 GMS

# A leftover row from an interrupted run would make the reserve below a 409 and
# report a failure that says nothing about this run. Releasing it is
# best-effort: a 404 here is the normal case.
icall POST /internal/v1/tenant-routes/release "$ZENSYS_TOKEN_POS" \
  "{\"tenant\":\"$LIFECYCLE_SLUG\",\"confirm\":\"$LIFECYCLE_SLUG\"}" >/dev/null 2>&1 || true

WRONG_TOKEN="$(printf 'c%.0s' $(seq 1 32))"

echo
echo "-- authentication (§41) --"
check "no token at all"        "401" "$(icode POST /internal/v1/tenant-routes/reserve "")"
check "an unknown token"       "401" "$(icode POST /internal/v1/tenant-routes/reserve "$WRONG_TOKEN" \
                                         '{"tenant":"probe-lc-9","product":"POS"}')"
check "a listing needs a token" "401" "$(icode GET /internal/v1/tenant-routes "")"
check "a wrong scheme"         "401" "$(istatus /internal/v1/tenant-routes -H "Authorization: Basic $WRONG_TOKEN")"
check "two Authorization headers" "401" "$(istatus /internal/v1/tenant-routes \
                                              -H "Authorization: Bearer $ZENSYS_TOKEN_POS" \
                                              -H "Authorization: Bearer $ZENSYS_TOKEN_GMS")"

echo
echo "-- the policy document (§10) --"
check "platform-policy" "200" "$(icode GET /internal/v1/platform-policy "$ZENSYS_TOKEN_POS")"
icall GET /internal/v1/platform-policy "$ZENSYS_TOKEN_POS"
check_contains "the slug lengths are published" '"min_length":3,"max_length":63' "$ICALL_BODY"
check_contains "the reserved hosts are published" '"reserved_hosts"' "$ICALL_BODY"
check_contains "and include the platform hosts" '"www"' "$ICALL_BODY"

# The apex label is deliberately absent, and that is not an oversight. The apex
# is the bare root domain ("zensys.lk"); "zensys.zensys.lk" is a legitimate
# tenant host whose slug is "zensys", and reserving it works and routes. Listing
# it as reserved would make this document disagree with reserve — the exact
# drift the endpoint exists to prevent.
if printf '%s' "$ICALL_BODY" | grep -q '"zensys"'; then
  printf '  FAIL %-52s %s\n' "the apex label is not listed as reserved" "found, which contradicts reserve"
  FAILURES=$((FAILURES + 1))
else
  printf '  ok   %-52s %s\n' "the apex label is not listed as reserved" "absent, as intended"
fi

echo
echo "-- authorization: a POS token cannot touch GMS (§42) --"
check "reserve a GMS tenant"  "403" "$(icode POST /internal/v1/tenant-routes/reserve "$ZENSYS_TOKEN_POS" \
                                          '{"tenant":"probe-gms-1","product":"GMS"}')"
check "activate a GMS tenant" "403" "$(icode POST /internal/v1/tenant-routes/activate "$ZENSYS_TOKEN_POS" \
                                          '{"tenant":"garage1","product":"GMS"}')"
check "disable a GMS tenant"  "403" "$(icode POST /internal/v1/tenant-routes/disable "$ZENSYS_TOKEN_POS" \
                                          '{"tenant":"garage1"}')"
check "release a GMS tenant"  "403" "$(icode POST /internal/v1/tenant-routes/release "$ZENSYS_TOKEN_POS" \
                                          '{"tenant":"garage1","confirm":"garage1"}')"
check "read a GMS tenant"     "403" "$(icode GET /internal/v1/tenant-routes/garage1 "$ZENSYS_TOKEN_POS")"
check "list GMS routes"       "403" "$(icode GET '/internal/v1/tenant-routes?product=GMS' "$ZENSYS_TOKEN_POS")"

# The refusals must not have changed anything. A 403 that still performed the
# mutation would satisfy every check above.
check_contains "the GMS tenant is untouched" "upstream=gms-frontend" "$(body garage1.zensys.lk /)"

echo
echo "-- the private listener is the only way in (§40) --"
# With a tenant host, an /internal path belongs to the product: the Gateway must
# not have reserved it. The discriminating observable is the *product's*
# identity in the response — a 200 alone could have been either.
check_contains "the public listener proxies /internal to the product, not to the API" \
  "upstream=pos-frontend" "$(body mangala.zensys.lk /internal/v1/tenant-routes)"
check "and an unknown host gets the ordinary 404" \
  "404" "$(status nobody.zensys.lk /internal/v1/tenant-routes)"

# The same path on the private port is the API, and refuses without a token —
# which is what shows the two listeners really are different handlers.
check "the same path on the private port is the API" "401" "$(icode GET /internal/v1/tenant-routes "")"

echo
echo "-- the round trip, and whether it takes effect immediately (§33) --"
echo "   (route_ttl and negative_ttl are $LONG_TTL here, so nothing converges on its own)"

# A public request for the slug before it exists caches the *absence*, which is
# the harder case to invalidate: a plain route cache would be cleared by the
# reservation, but the negative entry is keyed on a slug that had no row.
check "the slug does not resolve yet" "404" "$(status "$LIFECYCLE_SLUG.zensys.lk" /)"

icall POST /internal/v1/tenant-routes/reserve "$ZENSYS_TOKEN_POS" \
  "{\"tenant\":\"$LIFECYCLE_SLUG\",\"product\":\"POS\"}"
check "reserve" "201" "$ICALL_STATUS"
check_contains "a new route is PROVISIONING" '"status":"PROVISIONING"' "$ICALL_BODY"
check_contains "and the response names the product" '"product":"POS"' "$ICALL_BODY"

check "a duplicate reserve is a conflict" "409" "$(icode POST /internal/v1/tenant-routes/reserve "$ZENSYS_TOKEN_POS" \
  "{\"tenant\":\"$LIFECYCLE_SLUG\",\"product\":\"POS\"}")"

# PROVISIONING is not routable, and the answer must be indistinguishable from an
# unknown tenant's.
check "a reserved tenant is not routable yet" "404" "$(status "$LIFECYCLE_SLUG.zensys.lk" /)"

check "activate" "200" "$(icode POST /internal/v1/tenant-routes/activate "$ZENSYS_TOKEN_POS" \
  "{\"tenant\":\"$LIFECYCLE_SLUG\",\"product\":\"POS\"}")"

# The criterion. No sleep, no retry: the very next request must already reflect
# the change, and with a 300s TTL there is no other way it could.
check "the activated tenant routes on the very next request" "200" "$(status "$LIFECYCLE_SLUG.zensys.lk" /)"
check_contains "and reaches the POS frontend" "upstream=pos-frontend" "$(body "$LIFECYCLE_SLUG.zensys.lk" /)"

check "disable" "200" "$(icode POST /internal/v1/tenant-routes/disable "$ZENSYS_TOKEN_POS" \
  "{\"tenant\":\"$LIFECYCLE_SLUG\"}")"
check "a disabled tenant stops routing on the very next request" \
  "404" "$(status "$LIFECYCLE_SLUG.zensys.lk" /)"

# The disabled 404 must be byte-identical to the unknown-tenant 404, or tenant
# existence leaks through the body.
check "a disabled tenant is indistinguishable from an unknown one" \
  "$(body nobody.zensys.lk /)" "$(body "$LIFECYCLE_SLUG.zensys.lk" /)"

echo
echo "-- release is deliberate (§38) --"
check "release without a confirmation" "400" "$(icode POST /internal/v1/tenant-routes/release "$ZENSYS_TOKEN_POS" \
  "{\"tenant\":\"$LIFECYCLE_SLUG\"}")"
check "a mismatched confirmation"      "400" "$(icode POST /internal/v1/tenant-routes/release "$ZENSYS_TOKEN_POS" \
  "{\"tenant\":\"$LIFECYCLE_SLUG\",\"confirm\":\"something-else\"}")"
check "a confirmation naming the product" "400" "$(icode POST /internal/v1/tenant-routes/release "$ZENSYS_TOKEN_POS" \
  "{\"tenant\":\"$LIFECYCLE_SLUG\",\"confirm\":\"POS\"}")"
# The route must have survived all three refusals. Re-enabling it first makes
# this observable: a 200 means the row is still there and still usable.
check "the route survived the refusals" "200" "$(icode POST /internal/v1/tenant-routes/activate "$ZENSYS_TOKEN_POS" \
  "{\"tenant\":\"$LIFECYCLE_SLUG\",\"product\":\"POS\"}")"

check "a matching confirmation releases it" "200" "$(icode POST /internal/v1/tenant-routes/release "$ZENSYS_TOKEN_POS" \
  "{\"tenant\":\"$LIFECYCLE_SLUG\",\"confirm\":\"$LIFECYCLE_SLUG\"}")"
check "a released tenant is gone" "404" "$(status "$LIFECYCLE_SLUG.zensys.lk" /)"

echo
echo "-- reads --"
check "get a route" "200" "$(icode GET /internal/v1/tenant-routes/mangala "$ZENSYS_TOKEN_POS")"
icall GET /internal/v1/tenant-routes/mangala "$ZENSYS_TOKEN_POS"
check_contains "the route carries its status" '"status":"ACTIVE"' "$ICALL_BODY"
check "get an unknown route" "404" "$(icode GET /internal/v1/tenant-routes/nobody "$ZENSYS_TOKEN_POS")"
check "a POS token lists only POS routes" "200" "$(icode GET /internal/v1/tenant-routes "$ZENSYS_TOKEN_POS")"
icall GET /internal/v1/tenant-routes "$ZENSYS_TOKEN_POS"
if printf '%s' "$ICALL_BODY" | grep -q '"product":"GMS"'; then
  printf '  FAIL %-52s %s\n' "the listing leaked a GMS route" "$ICALL_BODY"
  FAILURES=$((FAILURES + 1))
else
  printf '  ok   %-52s %s\n' "the listing carries no GMS route" "clean"
fi

echo
echo "-- error contract --"
check "an unknown endpoint under the prefix" "404" "$(icode GET /internal/v1/nope "$ZENSYS_TOKEN_POS")"
check "a wrong method names the right verb"  "405" "$(icode PUT /internal/v1/platform-policy "$ZENSYS_TOKEN_POS")"

echo
echo "-- the audit trail names the caller --"
# Every mutation in this section carried the POS token, so the log must say so.
# The token itself must not appear anywhere.
if grep -q 'service=pos' configs/.probe/f.log || grep -q '"service":"pos"' configs/.probe/f.log; then
  printf '  ok   %-52s %s\n' "the access log names the calling service" "service=pos"
else
  printf '  FAIL %-52s %s\n' "the access log names the calling service" "no service field found"
  FAILURES=$((FAILURES + 1))
fi

if grep -qF "$ZENSYS_TOKEN_POS" configs/.probe/f.log; then
  printf '  FAIL %-52s %s\n' "a service token was written to the log" "token found in f.log"
  FAILURES=$((FAILURES + 1))
else
  printf '  ok   %-52s %s\n' "no service token was written to the log" "absent"
fi

# The refusals must be visible without debug logging, and the reasons must be
# distinguishable in the log even though the responses are identical.
for pair in "an unauthenticated call|not authenticated" \
            "a refused product|not authorized for this product"; do
  label="${pair%%|*}"; needle="${pair#*|}"
  if grep -qF "$needle" configs/.probe/f.log; then
    printf '  ok   %-52s %s\n' "$label is logged" "$needle"
  else
    printf '  FAIL %-52s %s\n' "$label is logged" "not found in f.log"
    FAILURES=$((FAILURES + 1))
  fi
done

# The round trip must have left the registry as it found it.
check "the fixtures are still routable" "200" "$(status mangala.zensys.lk /)"

stop

echo
echo "=============================================================="
echo "G. rate limiting: tier 2, per tenant (§81, plan §8.2)"
echo "=============================================================="

start "$G" configs/.probe/g.log || exit 1

# Both buckets are configured with a 100-second refill, so nothing earned
# during this section can make a refusal succeed. That is what turns "the third
# request is refused" into a statement about the burst rather than a race with
# the clock.
echo
echo "-- startup states the configured limits --"
grep -o '"msg":"rate limiting configured"[^}]*' configs/.probe/g.log | head -1 \
  || echo "(the configured limits were not logged)"

echo
echo "-- tier 2 is keyed by tenant, and fires after resolution --"
# Tier 2's burst is 2 and tier 1's is 200, so a 429 here can only be tier 2.
check "mangala 1st"          "200" "$(status mangala.zensys.lk /)"
check "mangala 2nd"          "200" "$(status mangala.zensys.lk /)"
check "mangala 3rd refused"  "429" "$(status mangala.zensys.lk /)"

echo
echo "-- the refusal is the error contract, with a usable Retry-After --"
check_contains "the refusal names the rate-limit error" '"error":"rate_limited"' \
  "$(body mangala.zensys.lk /)"

# Retry-After is read from a single response, so the header and the status
# belong to the same decision.
G_RETRY="$(resp_header mangala.zensys.lk / Retry-After)"
if [ -z "$G_RETRY" ]; then
  printf '  FAIL %-52s %s\n' "the refusal carries Retry-After" "absent"
  FAILURES=$((FAILURES + 1))
elif [ "$G_RETRY" = "0" ]; then
  printf '  FAIL %-52s %s\n' "Retry-After is not zero" "0 — an instruction to retry immediately"
  FAILURES=$((FAILURES + 1))
else
  printf '  ok   %-52s %s\n' "the refusal carries a non-zero Retry-After" "$G_RETRY"
fi

echo
echo "-- a refused request never reaches a product --"
if printf '%s' "$(body mangala.zensys.lk /)" | grep -q 'upstream='; then
  printf '  FAIL %-52s %s\n' "the refused request reached an upstream" "an upstream answered it"
  FAILURES=$((FAILURES + 1))
else
  printf '  ok   %-52s %s\n' "the refused request did not reach an upstream" "Gateway error only"
fi

echo
echo "-- another tenant has its own allowance --"
# The whole point of the second tier: one tenant exhausting its bucket must not
# affect its neighbour.
check "garage1 1st" "200" "$(status garage1.zensys.lk /)"
check "garage1 2nd" "200" "$(status garage1.zensys.lk /)"

echo
echo "-- a tier-2 refusal is distinguishable in the log by its tenant --"
# Tier 1 runs in the middleware chain and tier 2 runs inside the routing
# handler, so a 429 line that carries a tenant is tier 2 and one that does not
# is tier 1. That is structural, not conventional.
if grep '"status":429' configs/.probe/g.log | grep -q '"tenant":"mangala"'; then
  printf '  ok   %-52s %s\n' "the tier-2 refusal names the tenant" 'tenant=mangala'
else
  printf '  FAIL %-52s %s\n' "the tier-2 refusal names the tenant" "no 429 line with a tenant"
  FAILURES=$((FAILURES + 1))
fi

stop

echo
echo "=============================================================="
echo "H. rate limiting: tier 1, per client address (§81)"
echo "=============================================================="

start "$H" configs/.probe/h.log || exit 1

echo
echo "-- operational paths are exempt from tier 1 --"
# Tier 1's burst is 2. Five hits on a health endpoint must all be served; if
# the exemption were missing, the third would be refused. A probe refused for
# being too frequent reports the service as dead, and an orchestrator that
# restarts on a failed probe turns a load spike into an outage.
EXEMPT_OK=0
for _ in $(seq 1 5); do
  if [ "$(status_h mangala.zensys.lk /healthz)" = "200" ]; then
    EXEMPT_OK=$((EXEMPT_OK + 1))
  fi
done
check "5 of 5 exempt health checks served" "5" "$EXEMPT_OK"

echo
echo "-- the key is the address the trusted-proxy policy resolved --"
# 127.0.0.1 is a configured trusted proxy, so the forwarded address is believed
# and becomes the key. Two different forwarded clients must get two buckets.
check "client 203.0.113.1, 1st"  "200" "$(status_h mangala.zensys.lk / 'X-Forwarded-For: 203.0.113.1')"
check "client 203.0.113.1, 2nd"  "200" "$(status_h mangala.zensys.lk / 'X-Forwarded-For: 203.0.113.1')"
check "client 203.0.113.1, 3rd"  "429" "$(status_h mangala.zensys.lk / 'X-Forwarded-For: 203.0.113.1')"

check "a second client is unaffected" "200" \
  "$(status_h mangala.zensys.lk / 'X-Forwarded-For: 203.0.113.2')"

echo
echo "-- a tier-1 refusal happens before routing, so it names no tenant --"
# The other half of the structural distinction. Tier 1 refuses in the
# middleware chain, before the hostname is parsed, so there is no tenant to
# report — and emitting an empty one would make "not routed" indistinguishable
# from "routed to a tenant with an empty slug".
if grep '"status":429' configs/.probe/h.log | grep -v '"tenant"' | grep -q '"status":429'; then
  printf '  ok   %-52s %s\n' "the tier-1 refusal names no tenant" "absent"
else
  printf '  FAIL %-52s %s\n' "the tier-1 refusal names no tenant" "every 429 line carried a tenant"
  FAILURES=$((FAILURES + 1))
fi

check_contains "the tier-1 refusal is the error contract" '"error":"rate_limited"' \
  "$(body_h mangala.zensys.lk / 'X-Forwarded-For: 203.0.113.1')"

stop

echo
echo "=============================================================="
echo "I. rate limiting: a rotated X-Forwarded-For buys nothing (§81)"
echo "=============================================================="

start "$I" configs/.probe/i.log || exit 1

# Nothing is trusted in this config, so the forwarded header is both ignored
# and deleted, and every request is attributed to the socket. If the limiter
# keyed on the header instead, each request below would be handed a fresh
# bucket and the limit would never fire — the tier would exist only in the
# configuration file.
echo
echo "-- rotating the forwarded address does not grant a fresh bucket --"
check "spoofed client A, 1st" "200" "$(status_h mangala.zensys.lk / 'X-Forwarded-For: 198.51.100.1')"
check "spoofed client B, 2nd" "200" "$(status_h mangala.zensys.lk / 'X-Forwarded-For: 198.51.100.2')"
check "spoofed client C, 3rd" "429" "$(status_h mangala.zensys.lk / 'X-Forwarded-For: 198.51.100.3')"

stop

echo
echo "=============================================================="
echo "J. size limits: an oversized header block is in the contract (§81)"
echo "=============================================================="

start "$K" configs/.probe/k.log || exit 1

# The limit is 1024 bytes and the header sent is 2048, which is well under
# net/http's hard cap of limit + 4096. So this request is refused by the
# Gateway's own check, in the error contract, and logged — rather than by
# net/http, which would answer a bare text 431 straight to the connection
# before any Gateway code runs, and log nothing.
echo
echo "-- an oversized header block is refused in the error contract --"
BIG_HEADER="X-Big: $(repeat_char A "$OVERSIZED_HEADER")"
check "oversized header" "431" "$(status_h mangala.zensys.lk / "$BIG_HEADER")"
check_contains "and names the right error" '"error":"headers_too_large"' \
  "$(body_h mangala.zensys.lk / "$BIG_HEADER")"

echo
echo "-- the refusal is logged, which net/http's own would not be --"
if grep -q '"status":431' configs/.probe/k.log; then
  printf '  ok   %-52s %s\n' "the 431 reached the access log" "status=431"
else
  printf '  FAIL %-52s %s\n' "the 431 reached the access log" "no 431 line in k.log"
  FAILURES=$((FAILURES + 1))
fi

echo
echo "-- a header limit is not a body limit --"
# The rate-limit buckets in this config are the shipped, generous ones, so a
# 200 here says the ordinary request was served rather than refused for some
# other reason. That is what makes the 431 above attributable to the header.
check "an ordinary request is still served" "200" "$(status mangala.zensys.lk /)"
check "a tenant under /api is still served"  "200" "$(status mangala.zensys.lk /api/orders)"

stop

echo
echo "=============================================================="
echo "D. one product down, the other untouched (§48)"
echo "=============================================================="

stop_pos_upstreams

start "$A" configs/.probe/d.log || exit 1

echo
echo "-- a POS tenant gets the Gateway's error, not another product's answer --"
check "POS tenant status" "502" "$(status mangala.zensys.lk /)"
check_contains "POS tenant body names the upstream, not a product" \
  '"error":"upstream_unavailable"' "$(body mangala.zensys.lk /)"

echo
echo "-- a GMS tenant in the same process is unaffected --"
check "GMS tenant status" "200" "$(status garage1.zensys.lk /)"
check_contains "GMS tenant still reaches its own frontend" \
  "upstream=gms-frontend" "$(body garage1.zensys.lk /)"

echo
echo "-- nothing was served from the other product --"
# §48's real assertion. A 502 for POS is only reassuring if the answer did not
# come from GMS, and the only way to show that is to look for GMS's identity in
# the response POS received.
POS_BODY="$(body mangala.zensys.lk /)"
if printf '%s' "$POS_BODY" | grep -q 'upstream=gms'; then
  printf '  FAIL %-52s %s\n' "the POS request was answered by a GMS upstream" "$POS_BODY"
  FAILURES=$((FAILURES + 1))
else
  printf '  ok   %-52s %s\n' "the POS request was not answered by a GMS upstream" "$POS_BODY"
fi

stop

echo
echo "=============================================================="
if [ "$FAILURES" -eq 0 ]; then
  echo "RESULT: all assertions passed"
else
  echo "RESULT: $FAILURES assertion(s) FAILED"
fi
echo "=============================================================="

exit $([ "$FAILURES" -eq 0 ] && echo 0 || echo 1)
