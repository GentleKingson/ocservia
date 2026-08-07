SHELL := /usr/bin/env bash

.DEFAULT_GOAL := verify

.PHONY: bootstrap generate format lint test verify docs-check generated-clean generated-clean-test policy-check contracts-breaking go-check rust-check web-check security-check license-check database-integration integration e2e p1-smoke p1-full

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

go-check:
	./scripts/go-check.sh

database-integration:
	./scripts/database-integration.sh

integration:
	./scripts/local-slice-integration.sh

e2e:
	./scripts/e2e.sh

p1-smoke:
	P1_PROFILE=smoke AGENT_COUNT=24 HEARTBEAT_COUNT=2 HEARTBEAT_INTERVAL_MS=500 REQUEST_CONCURRENCY=8 QUEUE_CAPACITY=256 MINIMUM_RESOURCE_SAMPLES=8 ./scripts/p1-resilience-capacity.sh

p1-full:
	P1_PROFILE=full ./scripts/p1-resilience-capacity.sh

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

generated-clean-test:
	./scripts/test-generated-clean.sh

policy-check:
	./scripts/check-public-repository.sh

contracts-breaking:
	./scripts/check-breaking.sh
