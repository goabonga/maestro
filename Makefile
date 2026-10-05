# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Chris <goabonga@pm.me>

SHELL := /bin/bash
MULTICZ := uv tool run --with multicz-go-deps-plugin multicz
BIN := bin

.PHONY: check build scripts-check docs icons license-check release-plan release-validate go-test go-check

check: license-check release-validate scripts-check go-check
	python3 -c 'import tomllib; tomllib.load(open("zensical.toml", "rb"))'

build:
	go build -trimpath -o $(BIN)/maestro ./cmd/cli
	go build -trimpath -o $(BIN)/maestro-svc ./cmd/svc

scripts-check:
	python3 -m compileall -q scripts -x '/\.venv/'
	uv run --project scripts --locked pytest

docs:
	uv tool run zensical==0.0.67 build --clean

icons:
	python3 scripts/regen_icons.py

release-plan:
	$(MULTICZ) plan

release-validate:
	$(MULTICZ) validate --strict

license-check:
	python3 scripts/add_license_header.py --path cmd --types go --check
	python3 scripts/add_license_header.py --path internal --types go --check
	python3 scripts/add_license_header.py --path scripts --types py,toml --check
	python3 scripts/add_license_header.py --path .github --types yml,yaml,toml --check

go-test:
	go test -race -count=1 ./cmd/... ./internal/...

go-check: go-test
	go vet ./cmd/... ./internal/...
	go build ./cmd/...
	go run github.com/securego/gosec/v2/cmd/gosec@v2.29.0 ./cmd/... ./internal/...
