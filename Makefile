.PHONY: help build run test clean docker-build docker-run docker-stop deps lint fmt vet

# Variables
BINARY_NAME=gateway
DOCKER_IMAGE=inbound-gateway
VERSION?=latest
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod
GOFMT=gofmt
GOVET=$(GOCMD) vet
GOLINT=golangci-lint

# Build variables
LDFLAGS=-ldflags "-X main.Version=$(VERSION) -X main.BuildTime=$(shell date -u '+%Y-%m-%d_%H:%M:%S')"

help: ## Display this help screen
	@grep -h -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary
	$(GOBUILD) $(LDFLAGS) -o $(BINARY_NAME) -v cmd/gateway/main.go

run: ## Run the application
	$(GOBUILD) $(LDFLAGS) -o $(BINARY_NAME) -v cmd/gateway/main.go
	./$(BINARY_NAME)

test: ## Run tests
	$(GOTEST) -v -race -coverprofile=coverage.txt -covermode=atomic ./...

test-integration: ## Run integration tests
	$(GOTEST) -v -tags=integration ./...

coverage: test ## Generate coverage report
	$(GOCMD) tool cover -html=coverage.txt -o coverage.html
	@echo "Coverage report generated: coverage.html"

clean: ## Remove build artifacts
	$(GOCLEAN)
	rm -f $(BINARY_NAME)
	rm -f coverage.txt coverage.html

deps: ## Download dependencies
	$(GOMOD) download
	$(GOMOD) tidy

lint: ## Run linter
	@if command -v golangci-lint >/dev/null 2>&1; then \
		$(GOLINT) run ./...; \
	else \
		echo "golangci-lint not installed. Run: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
	fi

fmt: ## Format code
	$(GOFMT) -s -w .

vet: ## Run go vet
	$(GOVET) ./...

docker-build: ## Build Docker image
	docker build -t $(DOCKER_IMAGE):$(VERSION) .

docker-run: ## Run Docker container
	docker run -d -p 8080:8080 --name $(BINARY_NAME) $(DOCKER_IMAGE):$(VERSION)

docker-stop: ## Stop Docker container
	docker stop $(BINARY_NAME) || true
	docker rm $(BINARY_NAME) || true

docker-compose-up: ## Start all services with docker-compose
	docker-compose up -d

docker-compose-down: ## Stop all services with docker-compose
	docker-compose down

docker-compose-logs: ## View docker-compose logs
	docker-compose logs -f

# Development helpers
dev: ## Run in development mode with hot reload
	@if command -v air >/dev/null 2>&1; then \
		air; \
	else \
		echo "Air not installed. Run: go install github.com/cosmtrek/air@latest"; \
		$(MAKE) run; \
	fi

proto-gen: ## Generate protobuf files (if using gRPC)
	@echo "Generating protobuf files..."
	# protoc commands would go here

migrate-up: ## Run database migrations up
	@echo "Running migrations up..."
	# Migration commands would go here

migrate-down: ## Run database migrations down
	@echo "Running migrations down..."
	# Migration commands would go here

# CI/CD helpers
ci: deps lint vet test ## Run CI pipeline locally

install: ## Install the binary
	$(GOBUILD) $(LDFLAGS) -o $(GOPATH)/bin/$(BINARY_NAME) cmd/gateway/main.go

# Performance testing
bench: ## Run benchmarks
	$(GOTEST) -bench=. -benchmem ./...

load-test: ## Run load tests
	@echo "Running load tests..."
	@if command -v k6 >/dev/null 2>&1; then \
		k6 run tests/load/script.js; \
	else \
		echo "k6 not installed. Visit https://k6.io/docs/getting-started/installation"; \
	fi

# Security
security-scan: ## Run security scan
	@if command -v gosec >/dev/null 2>&1; then \
		gosec ./...; \
	else \
		echo "gosec not installed. Run: go install github.com/securego/gosec/v2/cmd/gosec@latest"; \
	fi

# Documentation
docs: ## Generate documentation
	@if command -v godoc >/dev/null 2>&1; then \
		echo "Documentation server running at http://localhost:6060"; \
		godoc -http=:6060; \
	else \
		echo "godoc not installed. Run: go install golang.org/x/tools/cmd/godoc@latest"; \
	fi

# Release
release: ## Create a new release
	@echo "Creating release $(VERSION)..."
	git tag -a v$(VERSION) -m "Release v$(VERSION)"
	git push origin v$(VERSION)
