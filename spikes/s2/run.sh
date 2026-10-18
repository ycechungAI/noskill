#!/bin/sh
# Build and start the S2 mock backend and proxies, run the checks, stop everything.
cd "$(dirname "$0")" || exit 1
mkdir -p out
go build -o out/mock ./cmd/mock && go build -o out/proxy ./cmd/proxy && go build -o out/bench ./cmd/bench || exit 1
out/mock > out/mock.log 2>&1 & M=$!
out/proxy > out/proxy.log 2>&1 & P=$!
out/proxy -listen 127.0.0.1:11437 -keepalive 1s > out/proxy-ka.log 2>&1 & K=$!
sleep 1
echo "## Protocol"; uv run --with openai python -u protocol.py
echo; echo "## Overhead"; out/bench latency -n 2000
echo; echo "## Cancel"; out/bench cancel -n 30
kill $M $P $K
