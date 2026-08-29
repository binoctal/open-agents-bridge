const https = require("https");
const fs = require("fs");
const path = require("path");
const os = require("os");
const crypto = require("crypto");
const { execSync } = require("child_process");

const PACKAGE_VERSION = require("./package.json").version;

const REPO_OWNER = "binoctal";
const REPO_NAME = "open-agents-bridge";
const BINARY_NAME = "open-agents-bridge";

function getPlatform() {
  const platform = os.platform();
  const arch = os.arch();

  const osMap = {
    darwin: "darwin",
    linux: "linux",
    win32: "windows",
  };

  const archMap = {
    x64: "amd64",
    arm64: "arm64",
  };

  const goos = osMap[platform];
  const goarch = archMap[arch];

  if (!goos || !goarch) {
    throw new Error(`Unsupported platform: ${platform}-${arch}`);
  }

  return { goos, goarch, ext: platform === "win32" ? ".exe" : "" };
}

function fetchJSON(url) {
  return new Promise((resolve, reject) => {
    https
      .get(
        url,
        { headers: { "User-Agent": "open-agents-bridge-npm" } },
        (res) => {
          if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
            return fetchJSON(res.headers.location).then(resolve, reject);
          }
          if (res.statusCode !== 200) {
            return reject(new Error(`HTTP ${res.statusCode} from ${url}`));
          }
          let data = "";
          res.on("data", (chunk) => (data += chunk));
          res.on("end", () => {
            try {
              resolve(JSON.parse(data));
            } catch (e) {
              reject(e);
            }
          });
        }
      )
      .on("error", reject);
  });
}

function fetchText(url) {
  return new Promise((resolve, reject) => {
    https
      .get(url, { headers: { "User-Agent": "open-agents-bridge-npm" } }, (res) => {
        if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
          return fetchText(res.headers.location).then(resolve, reject);
        }
        if (res.statusCode !== 200) {
          return reject(new Error(`HTTP ${res.statusCode} from ${url}`));
        }
        let data = "";
        res.on("data", (chunk) => (data += chunk));
        res.on("end", () => resolve(data));
      })
      .on("error", reject);
  });
}

function sha256(filePath) {
  return crypto.createHash("sha256").update(fs.readFileSync(filePath)).digest("hex");
}

// Verify the download against the release's own checksums.txt. A missing
// checksums.txt aborts rather than skipping the check: whoever can swap an
// asset can also make checksums.txt 404, so "skip when absent" verifies
// nothing at all. The bridge is a long-lived daemon that takes commands from
// a remote, and install time is the one cheap chance to confirm the bytes are
// the ones that were published.
async function verifyChecksum(release, asset, filePath) {
  const checksums = release.assets.find((a) => a.name === "checksums.txt");
  if (!checksums) {
    fs.unlinkSync(filePath);
    throw new Error(
      `Release ${release.tag_name} has no checksums.txt; refusing to install an unverified binary`
    );
  }

  const text = await fetchText(checksums.browser_download_url);
  const line = text
    .split("\n")
    .map((l) => l.trim().split(/\s+/))
    .find((parts) => parts[1] === asset.name || parts[1] === `*${asset.name}`);

  if (!line) {
    fs.unlinkSync(filePath);
    throw new Error(`checksums.txt does not list ${asset.name}`);
  }

  const expected = line[0].toLowerCase();
  const actual = sha256(filePath);
  if (expected !== actual) {
    fs.unlinkSync(filePath);
    throw new Error(
      `Checksum mismatch for ${asset.name}\n  expected: ${expected}\n  actual:   ${actual}`
    );
  }

  console.log(`Checksum verified (sha256 ${actual.slice(0, 16)}…)`);
}

function downloadFile(url, destPath) {
  return new Promise((resolve, reject) => {
    const file = fs.createWriteStream(destPath);
    https
      .get(url, (res) => {
        if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
          file.close();
          fs.unlinkSync(destPath);
          return downloadFile(res.headers.location, destPath).then(resolve, reject);
        }
        if (res.statusCode !== 200) {
          file.close();
          fs.unlinkSync(destPath);
          return reject(new Error(`HTTP ${res.statusCode} downloading ${url}`));
        }
        res.pipe(file);
        file.on("finish", () => {
          file.close(resolve);
        });
      })
      .on("error", (err) => {
        file.close();
        fs.unlinkSync(destPath);
        reject(err);
      });
  });
}

async function install() {
  const { goos, goarch, ext } = getPlatform();

  console.log(`Detecting platform: ${goos}/${goarch}`);

  // Fetch the release matching THIS package's version, not "latest". Pulling
  // latest means the npm version and the binary version have nothing to do
  // with each other: a lockfile pins the package but not what the package
  // downloads, so the same lockfile installs different binaries over time.
  const tag = `v${PACKAGE_VERSION}`;
  const apiUrl = `https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/tags/${tag}`;
  console.log(`Fetching release ${tag}...`);

  let release;
  try {
    release = await fetchJSON(apiUrl);
  } catch (err) {
    console.error(`Failed to fetch release ${tag} of ${REPO_OWNER}/${REPO_NAME}.`);
    console.error(`  Error: ${err.message}`);
    console.error(
      "  This package installs the binary built for its own version; it does not fall back to the latest release."
    );
    process.exit(1);
  }

  // Find matching asset
  const suffix = `${goos}_${goarch}`;
  const asset = release.assets.find((a) => a.name.includes(suffix));

  if (!asset) {
    console.error(`No binary found for ${suffix} in release ${release.tag_name}`);
    console.error(
      "Available assets:",
      release.assets.map((a) => a.name).join(", ")
    );
    process.exit(1);
  }

  // Download and extract
  const binDir = path.join(__dirname, "bin");
  if (!fs.existsSync(binDir)) {
    fs.mkdirSync(binDir, { recursive: true });
  }

  const binaryName = BINARY_NAME + ext;
  const binaryPath = path.join(binDir, binaryName);

  console.log(`Downloading ${asset.name}...`);

  const downloadPath = path.join(binDir, asset.name);
  await downloadFile(asset.browser_download_url, downloadPath);
  await verifyChecksum(release, asset, downloadPath);

  if (asset.name.endsWith(".tar.gz")) {
    const tmpArchive = downloadPath;

    // Extract the binary from tarball
    try {
      if (goos === "darwin" || goos === "linux") {
        execSync(`tar -xzf "${tmpArchive}" -C "${binDir}" "${binaryName}"`, {
          stdio: "pipe",
        });
      }
    } catch (e) {
      // Fallback: try extracting the entire archive
      execSync(`tar -xzf "${tmpArchive}" -C "${binDir}"`, { stdio: "pipe" });
    }
    fs.unlinkSync(tmpArchive);
  } else if (asset.name.endsWith(".zip")) {
    const tmpArchive = downloadPath;

    // Extract using built-in or system unzip
    try {
      execSync(`unzip -o "${tmpArchive}" "${binaryName}" -d "${binDir}"`, {
        stdio: "pipe",
      });
    } catch (e) {
      // Fallback: try powershell on Windows
      execSync(
        `powershell -command "Expand-Archive -Path '${tmpArchive}' -DestinationPath '${binDir}' -Force"`,
        { stdio: "pipe" }
      );
    }
    fs.unlinkSync(tmpArchive);
  } else {
    // Plain binary — already downloaded and verified, just put it in place.
    fs.renameSync(downloadPath, binaryPath);
  }

  // Make executable (unix)
  if (goos !== "windows") {
    fs.chmodSync(binaryPath, 0o755);
  }

  console.log(`Successfully installed open-agents-bridge ${release.tag_name}`);
}

install().catch((err) => {
  console.error("Installation failed:", err.message);
  process.exit(1);
});
