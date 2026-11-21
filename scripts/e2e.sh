#!/usr/bin/env bash
# End-to-end test of cpa-stego-guard on CLIProxyAPI v8 with a fake upstream.
#
# Usage: scripts/e2e.sh <CLIProxyAPI source dir | cli-proxy-api binary>
#
# Linux only, needs root (or unprivileged user namespaces) for unshare, plus
# go, python3 and openssl. The test runs in a private mount and network
# namespace: api.anthropic.com resolves to a local fake server inside it, and
# nothing outside the namespace is reachable or changed.
# Set CPA_E2E_KEEP=1 to keep the run directory with logs and captures.
set -euo pipefail

target="${1:?usage: scripts/e2e.sh <CLIProxyAPI source dir | binary>}"
script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
repo_dir="$(CDPATH= cd -- "$script_dir/.." && pwd)"
run_dir="$(mktemp -d "${TMPDIR:-/tmp}/cpa-stego-guard-e2e.XXXXXX")"

cleanup() {
  if [[ "${CPA_E2E_KEEP:-0}" == "1" ]]; then
    echo "run directory kept: $run_dir"
  else
    rm -rf "$run_dir"
  fi
}
trap cleanup EXIT

echo "==> build plugin"
mkdir -p "$run_dir/plugins" "$run_dir/auth"
(cd "$repo_dir" && CGO_ENABLED=1 go build -buildvcs=false -tags cshared -buildmode=c-shared \
  -o "$run_dir/plugins/cpa-stego-guard.so" ./cmd/cpa-stego-guard)
rm -f "$run_dir/plugins/cpa-stego-guard.h"

if [[ -d "$target" ]]; then
  echo "==> build CLIProxyAPI from $target"
  (cd "$target" && go build -o "$run_dir/cli-proxy-api" ./cmd/server)
  cpa_binary="$run_dir/cli-proxy-api"
else
  cpa_binary="$(CDPATH= cd -- "$(dirname -- "$target")" && pwd)/$(basename -- "$target")"
fi

echo "==> test CA and certificate for api.anthropic.com"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=cpa-stego-guard e2e CA" \
  -keyout "$run_dir/ca.key" -out "$run_dir/ca.pem" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -subj "/CN=api.anthropic.com" \
  -keyout "$run_dir/server.key" -out "$run_dir/server.csr" >/dev/null 2>&1
printf 'subjectAltName=DNS:api.anthropic.com\nextendedKeyUsage=serverAuth\n' >"$run_dir/san.ext"
openssl x509 -req -in "$run_dir/server.csr" -CA "$run_dir/ca.pem" -CAkey "$run_dir/ca.key" -CAcreateserial \
  -days 2 -extfile "$run_dir/san.ext" -out "$run_dir/server.pem" >/dev/null 2>&1
printf '127.0.0.1 localhost api.anthropic.com\n::1 localhost\n' >"$run_dir/hosts"

# A Claude OAuth credential (sk-ant-oat prefix) so CLIProxyAPI takes the OAuth
# path with CCH signing. It is only ever sent to the fake server.
cat >"$run_dir/auth/claude-e2e.json" <<'EOF'
{
  "type": "claude",
  "email": "e2e@example.invalid",
  "access_token": "sk-ant-oat01-e2e-dummy-token",
  "refresh_token": "sk-ant-ort01-e2e-dummy-refresh",
  "expired": "2099-01-01T00:00:00Z",
  "last_refresh": "2026-01-01T00:00:00Z",
  "account_uuid": "0b8f4c2e-5d4a-4c1e-9f7a-2e6d8c9b1a30",
  "organization_uuid": "5c1d7e2a-3b4f-4a6d-8e9f-1a2b3c4d5e6f"
}
EOF

echo "==> run inside a private mount + network namespace"
unshare -mn --propagation private python3 "$script_dir/e2e.py" --cpa "$cpa_binary" --workdir "$run_dir"
