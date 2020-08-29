#!/usr/bin/env bash

set -eux

cd "$(dirname "$0")/.."

export GITHUB_TOKEN=dummy
export HELLO=hello

go run ./cmd/github-comment post --dry-run -k hello
go run ./cmd/github-comment exec --dry-run -k hello -- echo foo
go run ./cmd/github-comment exec --dry-run -k hello -- false || true
