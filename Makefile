SHELL := /usr/bin/env bash

.DEFAULT_GOAL := test

.PHONY: bootstrap generate format lint test test-go test-rust test-web verify docs-check generated-clean policy-check contracts-breaking go-check rust-check web-check security-check license-check database-smoke e2e hygiene-check

bootstrap:
	./scripts/bootstrap.sh all

generate:
	./scripts/generate.sh

format:
	./scripts/format.sh

lint:
	./scripts/lint.sh

test:
	./scripts/test.sh

test-go:
	source ./scripts/env.sh && (cd control-plane && go test ./...) && (cd signer && go test ./...)

test-rust:
	source ./scripts/env.sh && cd rust && cargo test --workspace --all-features --lib --bins

test-web:
	source ./scripts/env.sh && cd web && npm test

go-check:
	./scripts/go-check.sh

database-smoke:
	./scripts/database-postgres-smoke.sh

e2e:
	./scripts/e2e.sh

rust-check:
	./scripts/rust-check.sh

web-check:
	./scripts/web-check.sh

security-check:
	./scripts/security-check.sh

license-check:
	./scripts/license-check.sh

verify:
	./scripts/verify.sh

docs-check:
	./scripts/docs-check.sh

hygiene-check:
	./scripts/test-hygiene-check.sh

generated-clean:
	./scripts/generated-clean.sh

policy-check:
	./scripts/check-public-repository.sh

contracts-breaking:
	./scripts/check-breaking.sh
