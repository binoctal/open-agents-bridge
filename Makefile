.PHONY: all build clean install test release-sign

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

# Sign a release's checksums.txt with the offline key and attach the
# signature to the same GitHub Release (docs/release-signing.md).
#   make release-sign TAG=v0.13.3
# Downloads the Release's own checksums.txt (the file users will verify),
# signs it, and uploads the signature. The private key is read from
# ~/.config/open-agents/release-signing/ed25519.key.
release-sign:
	@test -n "$(TAG)" || (echo "usage: make release-sign TAG=vX.Y.Z" && exit 1)
	@mkdir -p $(BUILD_DIR)/release-sign
	gh release download $(TAG) --repo binoctal/open-agents-bridge --pattern checksums.txt --dir $(BUILD_DIR)/release-sign --clobber
	go run ./scripts/release-sign sign -in $(BUILD_DIR)/release-sign/checksums.txt -out $(BUILD_DIR)/release-sign/checksums.txt.sig
	gh release upload $(TAG) --repo binoctal/open-agents-bridge $(BUILD_DIR)/release-sign/checksums.txt.sig --clobber
