#!/bin/sh
# Runs govulncheck and fails on any reachable vulnerability except the
# allowlisted ones below. Each entry needs a reason; remove it when fixed.
#
#   GO-2026-4887  Moby AuthZ plugin bypass (dockerd server-side; we only use the client)
#   GO-2026-4883  Moby plugin privilege validation (dockerd server-side; client unaffected)
set -eu
ALLOW="GO-2026-4887 GO-2026-4883"
out=$(go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 -format json ./...)
found=$(printf '%s\n' "$out" | jq -r 'select(.finding != null and (.finding.trace[0].function // "") != "") | .finding.osv' | sort -u)
bad=""
for id in $found; do
  case " $ALLOW " in
    *" $id "*) echo "allowlisted: $id" ;;
    *) bad="$bad $id" ;;
  esac
done
if [ -n "$bad" ]; then
  echo "reachable vulnerabilities:$bad" >&2
  go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./... >&2 || true
  exit 1
fi
echo "govulncheck: no unexpected vulnerabilities"
