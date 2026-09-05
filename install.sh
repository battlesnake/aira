#!/bin/sh

set -e

go build -o bin/ ./cmd/aira/...

./bin/aira install
