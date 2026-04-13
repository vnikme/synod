# Makefile — thin wrapper around deploy scripts.
# Canonical deployment: deploy/gcp/*.sh

.PHONY: setup deploy-sandbox deploy-orchestrator deploy-all build test

setup:
	bash deploy/gcp/00_setup.gcp.sh

build:
	bash deploy/gcp/01_build.sh

deploy-sandbox:
	bash deploy/gcp/02_deploy.sh sandbox

deploy-orchestrator:
	bash deploy/gcp/02_deploy.sh orchestrator

deploy-all:
	bash deploy/gcp/02_deploy.sh

test:
	cd orchestrator && go vet ./... && go test -v ./...
	cd sandbox && python -m pytest tests/ -v

test-go:
	cd orchestrator && go vet ./... && go test -v ./...

test-python:
	cd sandbox && python -m pytest tests/ -v
