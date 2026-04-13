# Makefile — thin wrapper around deploy scripts.
# Canonical deployment: deploy/gcp/*.sh

.PHONY: setup deploy-sandbox deploy-orchestrator deploy-all build

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
