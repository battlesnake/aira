#!/bin/bash

set -xe

go build -o bin/ ./cmd/aira/...

install bin/aira ~/.local/bin/aira

~/.local/bin/aira install
