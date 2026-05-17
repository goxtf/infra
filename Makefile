# Makefile for e2b-dev/infra fork
# Provides common development, build, and deployment targets

.PHONY: help build test lint fmt clean dev deploy-gcp deploy-aws

# Default target
.DEFAULT_GOAL := help

# Load environment variables if .envrc exists
ifneq (,$(wildcard .envrc))
  include .envrc
  export
endif

# Go settings
GO := go
GOFLAGS := -v
GO_BUILD_FLAGS := -ldflags "-s -w"
# Reduced timeout from 120s to 60s for faster feedback during local dev
GO_TEST_FLAGS := -race -timeout 60s

# Docker settings
DOCKER := docker
DOCKER_COMPOSE := docker compose

# Directories
CMD_DIR := ./cmd
PKG_DIR := ./pkg
INFRA_DIR := ./infra

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@sed -n 's/^## //p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/  /'

## build: Build all Go binaries
build:
	$(GO) build $(GO_BUILD_FLAGS) $(GOFLAGS) ./...

## test: Run all tests
test:
	$(GO) test $(GO_TEST_FLAGS) ./...

## test-short: Run short tests only
test-short:
	$(GO) test -short $(GO_TEST_FLAGS) ./...

## lint: Run golangci-lint
lint:
	@which golangci-lint > /dev/null || (echo "golangci-lint not found, install from https://golangci-lint.run" && exit 1)
	golangci-lint run ./...

## fmt: Format Go source files
fmt:
	$(GO) fmt ./...
	goimports -w .

## vet: Run go vet
vet:
	$(GO) vet ./...

## tidy: Tidy go modules
tidy:
	$(GO) mod tidy

## clean: Remove build artifacts
clean:
	@rm -rf ./bin ./dist
	$(GO) clean ./...

## dev: Start local development environment
dev:
	$(DOCKER_COMPOSE) up --build

## dev-down: Stop local development environment
dev-down:
	$(DOCKER_COMPOSE) down

## deploy-gcp: Deploy infrastructure to GCP
deploy-gcp:
	@test -f .env.gcp || (echo ".env.gcp not found, copy from .env.gcp.template" && exit 1)
	@echo "Deploying to GCP..."
	cd $(INFRA_DIR) && terraform init && terraform apply -var-file=gcp.tfvars

## deploy-aws: Deploy infrastructure to AWS
deploy-aws:
	@test -f .env.aws || (echo ".env.aws not found, copy from .env.aws.template" && exit 1)
	@echo "Deploying to AWS..."
	cd $(INFRA_DIR) && terraform init && terraform apply -var-file=aws.tfvars

## docker-build: Build all Docker images
docker-build:
	$(DOCKER) build -t infra:latest .

## generate: Run go generate
generate:
	$(GO) generate ./...

## check: Run all checks (fmt, vet, lint, test)
check: fmt vet lint test

## ci: Run CI pipeline steps
ci: tidy generate build vet test
