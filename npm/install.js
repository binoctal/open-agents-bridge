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

function fetchBuffer(url) {
  return new Promise((resolve, reject) => {
    https
      .get(url, { headers: { "User-Agent": "open-agents-bridge-npm" } }, (res) => {
        if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
          return fetchBuffer(res.headers.location).then(resolve, reject);
        }
        if (res.statusCode !== 200) {
          return reject(new Error(`HTTP ${res.statusCode} from ${url}`));
        }
        const chunks = [];
        res.on("data", (chunk) => chunks.push(chunk));
        res.on("end", () => resolve(Buffer.concat(chunks)));
      })
      .on("error", reject);
  });
}

function sha256(filePath) {
  return crypto.createHash("sha256").update(fs.readFileSync(filePath)).digest("hex");
}

// Release signing keys (5e.2): base64 raw Ed25519 public keys. A list so a
// key can be rotated; keep in sync with trustedKeys in
// internal/updater/signature.go. See docs/release-signing.md.
const TRUSTED_KEYS = ["NTmWyMB+MN8QmQ+R0TXJ3t3cwuqki1Cxld9HzHzc4Vw="];

// Transitional policy: a release without checksums.txt.sig is refused while
// this is true.
const REQUIRE_SIGNATURE = true;

const SIGNATURE_ASSET = "checksums.txt.sig";

// DER SubjectPublicKeyInfo prefix for a raw Ed25519 public key.
const ED25519_SPKI_PREFIX = Buffer.from("302a300506032b6570032100", "hex");

function verifySignature(checksumsBuf, sigText, keys) {
  const sig = Buffer.from(String(sigText).trim(), "base64");
  if (sig.length !== 64) {
    throw new Error(`malformed ${SIGNATURE_ASSET}`);
  }
  for (const k of keys) {
    const raw = Buffer.from(k, "base64");
    if (raw.length !== 32) continue;
    const key = crypto.createPublicKey({
      key: Buffer.concat([ED25519_SPKI_PREFIX, raw]),
      format: "der",
      type: "spki",
    });
    if (crypto.verify(null, checksumsBuf, key, sig)) return;
  }
  throw new Error(`${SIGNATURE_ASSET} does not verify against any trusted release key`);
}

// Pure verification core. Order matters: the signature over checksums.txt is
// checked FIRST, then the archive hash against the authenticated checksums.
// Otherwise an attacker who can replace release assets replaces the archive
// and checksums.txt together. Throws on any failure; never deletes anything.
function verifyDownload({ checksumsBuf, sigText, assetName, filePath, keys, requireSignature }) {
  if (sigText == null) {
    if (requireSignature) {
      throw new Error(`Release has no ${SIGNATURE_ASSET}; refusing to install an unsigned binary`);
    }
  } else {
    verifySignature(checksumsBuf, sigText, keys);
  }

  const line = checksumsBuf
    .toString("utf8")
    .split("\n")
    .map((l) => l.trim().split(/\s+/))
    .find((parts) => parts[1] === assetName || parts[1] === `*${assetName}`);

  if (!line) {
    throw new Error(`checksums.txt does not list ${assetName}`);
  }

  const expected = line[0].toLowerCase();
  const actual = sha256(filePath);
  if (expected !== actual) {
    throw new Error(
      `Checksum mismatch for ${assetName}\n  expected: ${expected}\n  actual:   ${actual}`
    );
  }
  return actual;
}

// Verify the download against the release's signed checksums.txt. A missing
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
  const sigAsset = release.assets.find((a) => a.name === SIGNATURE_ASSET);

  try {
    const checksumsBuf = await fetchBuffer(checksums.browser_download_url);
    const sigText = sigAsset ? (await fetchBuffer(sigAsset.browser_download_url)).toString("utf8") : null;
    const actual = verifyDownload({
      checksumsBuf,
      sigText,
      assetName: asset.name,
      filePath,
      keys: TRUSTED_KEYS,
      requireSignature: REQUIRE_SIGNATURE,
    });
    console.log(`Signature and checksum verified (sha256 ${actual.slice(0, 16)}…)`);
  } catch (err) {
    fs.unlinkSync(filePath);
    throw err;
  }
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

if (require.main === module) {
  install().catch((err) => {
    console.error("Installation failed:", err.message);
    process.exit(1);
  });
}

module.exports = { verifySignature, verifyDownload, TRUSTED_KEYS };
