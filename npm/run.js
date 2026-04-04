#!/usr/bin/env node

const { spawn } = require("child_process");
const path = require("path");
const os = require("os");

const ext = os.platform() === "win32" ? ".exe" : "";
const binaryPath = path.join(__dirname, "bin", `open-agents-bridge${ext}`);

const child = spawn(binaryPath, process.argv.slice(2), {
  stdio: "inherit",
  env: process.env,
});

child.on("exit", (code) => {
  process.exit(code ?? 1);
});
