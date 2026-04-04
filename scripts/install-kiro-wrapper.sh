#!/bin/bash

# Install Kiro CLI wrapper for Open Agents

WRAPPER_PATH="$(dirname "$0")/kiro-wrapper.sh"
INSTALL_DIR="$HOME/.local/bin"
REAL_KIRO=$(which kiro-cli 2>/dev/null)

if [ -z "$REAL_KIRO" ]; then
  echo "❌ kiro-cli not found. Please install it first."
  exit 1
fi

echo "📦 Installing Open Agents wrapper for Kiro CLI..."

# Create install directory
mkdir -p "$INSTALL_DIR"

# Backup original kiro-cli
if [ -f "$INSTALL_DIR/kiro-cli.original" ]; then
  echo "⚠️  Backup already exists"
else
  echo "💾 Backing up original kiro-cli..."
  cp "$REAL_KIRO" "$INSTALL_DIR/kiro-cli.original"
fi

# Install wrapper
echo "📝 Installing wrapper..."
cp "$WRAPPER_PATH" "$INSTALL_DIR/kiro-cli"
chmod +x "$INSTALL_DIR/kiro-cli"

# Update PATH
if ! echo "$PATH" | grep -q "$INSTALL_DIR"; then
  echo ""
  echo "⚠️  Add this to your ~/.bashrc or ~/.zshrc:"
  echo "export PATH=\"$INSTALL_DIR:\$PATH\""
  echo ""
fi

echo "✅ Installation complete!"
echo ""
echo "Usage:"
echo "  kiro-cli chat \"your prompt\"  # Will request permission via Open Agents"
echo ""
echo "Uninstall:"
echo "  rm $INSTALL_DIR/kiro-cli"
echo "  mv $INSTALL_DIR/kiro-cli.original $INSTALL_DIR/kiro-cli"
