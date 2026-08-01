SHELL := /usr/bin/env bash

.DEFAULT_GOAL := verify

.PHONY: bootstrap generate format lint test verify docs-check generated-clean policy-check contracts-breaking go-check rust-check web-check security-check license-check

bootstrap:
	./scripts/bootstrap.sh

generate:
	./scripts/generate.sh

format:
	./scripts/format.sh

lint:
	./scripts/lint.sh

test:
	./scripts/test.sh

go-check:
	./scripts/go-check.sh

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

generated-clean:
	./scripts/generated-clean.sh

policy-check:
	./scripts/check-public-repository.sh

contracts-breaking:
	./scripts/check-breaking.sh
