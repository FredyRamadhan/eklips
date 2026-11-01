#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

mkdir -p bench
CURRENT="bench/current.txt"
BASELINE="bench/baseline.txt"

echo "==> Running benchmarks..."
go test -run=^$ -bench=. -benchmem -count=3 ./internal/audio ./internal/sink ./internal/engine ./internal/store ./internal/tui | tee "$CURRENT"

if [[ ! -f "$BASELINE" ]]; then
  cp "$CURRENT" "$BASELINE"
  echo "==> Created initial benchmark baseline at $BASELINE"
  exit 0
fi

if command -v benchstat >/dev/null 2>&1; then
  echo "==> Comparing against $BASELINE with benchstat..."
  benchstat "$BASELINE" "$CURRENT"
else
  echo "==> benchstat not installed; benchmark results saved to $CURRENT"
fi
