#!/bin/bash

# Inbound Gateway Quick Start Script
# This script helps you get started with the Inbound Gateway Service

set -e

echo "==================================="
echo "Inbound Gateway Service Quick Start"
echo "==================================="
echo ""

# Check if Go is installed
if ! command -v go &> /dev/null; then
    echo "❌ Go is not installed. Please install Go 1.21+ first."
    echo "   Visit: https://golang.org/doc/install"
    exit 1
fi

echo "✅ Go is installed: $(go version)"

# Check if Docker is installed (optional)
if command -v docker &> /dev/null; then
    echo "✅ Docker is installed: $(docker --version)"
    DOCKER_AVAILABLE=true
else
    echo "⚠️  Docker is not installed (optional for containerized deployment)"
    DOCKER_AVAILABLE=false
fi

echo ""
echo "Choose how to run the gateway:"
echo "1) Local development (go run)"
echo "2) Build and run binary"
if [ "$DOCKER_AVAILABLE" = true ]; then
    echo "3) Run with Docker"
    echo "4) Run with Docker Compose (includes Redis, monitoring)"
fi
echo ""

read -p "Enter your choice (1-4): " choice

case $choice in
    1)
        echo ""
        echo "🚀 Starting gateway in development mode..."
        echo ""
        
        # Install dependencies
        echo "Installing dependencies..."
        go mod download
        
        # Set default environment variables for local development
        export GCP_PROJECT_ID=${GCP_PROJECT_ID:-"local-development"}
        export PUBSUB_EMULATOR_HOST=${PUBSUB_EMULATOR_HOST:-"localhost:8085"}
        
        echo "Starting gateway on port 8080..."
        go run cmd/gateway/main.go -config config/config.yaml
        ;;
        
    2)
        echo ""
        echo "🔨 Building gateway binary..."
        make build
        
        echo ""
        echo "🚀 Starting gateway..."
        ./gateway -config config/config.yaml
        ;;
        
    3)
        if [ "$DOCKER_AVAILABLE" = true ]; then
            echo ""
            echo "🐳 Building Docker image..."
            make docker-build
            
            echo ""
            echo "🚀 Starting gateway in Docker..."
            make docker-run
            
            echo ""
            echo "Gateway is running at http://localhost:8080"
            echo "View logs with: docker logs gateway"
        else
            echo "Docker is not available"
            exit 1
        fi
        ;;
        
    4)
        if [ "$DOCKER_AVAILABLE" = true ]; then
            echo ""
            echo "🐳 Starting services with Docker Compose..."
            make docker-compose-up
            
            echo ""
            echo "Services started:"
            echo "  - Gateway: http://localhost:8080"
            echo "  - Redis: localhost:6379"
            echo "  - Prometheus: http://localhost:9090"
            echo "  - Grafana: http://localhost:3000 (admin/admin)"
            echo ""
            echo "View logs with: make docker-compose-logs"
            echo "Stop services with: make docker-compose-down"
        else
            echo "Docker is not available"
            exit 1
        fi
        ;;
        
    *)
        echo "Invalid choice"
        exit 1
        ;;
esac

echo ""
echo "==================================="
echo "Quick Test Commands:"
echo "==================================="
echo ""
echo "# Health check:"
echo "curl http://localhost:8080/health"
echo ""
echo "# Send a test integration request:"
echo 'curl -X POST http://localhost:8080/v1/integrations/uber \'
echo '  -H "Content-Type: application/json" \'
echo '  -H "X-API-Key: demo-api-key-123" \'
echo '  -d '"'"'{"pickup_location": "123 Main St", "destination": "456 Oak Ave"}'"'"
echo ""
echo "# View metrics:"
echo "curl http://localhost:8080/metrics"
echo ""
