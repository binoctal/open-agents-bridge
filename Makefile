.PHONY: all build clean install test

BINARY_NAME=open-agents-bridge
BUILD_DIR=build

# Default build - build for current platform to build directory
all:
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/open-agents-bridge

# Build for current platform (legacy - outputs to root dir)
build:
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/open-agents-bridge

# Build for all platforms
build-all: build-linux build-darwin build-windows

build-linux:
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/open-agents-bridge
	GOOS=linux GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 ./cmd/open-agents-bridge

build-darwin:
	GOOS=darwin GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 ./cmd/open-agents-bridge
	GOOS=darwin GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 ./cmd/open-agents-bridge

build-windows:
	GOOS=windows GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe ./cmd/open-agents-bridge

# Install to /usr/local/bin
install: build
	cp $(BUILD_DIR)/$(BINARY_NAME) /usr/local/bin/

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR)

# Run tests
test:
	go test -v ./...

# Download dependencies
deps:
	go mod download
	go mod tidy
