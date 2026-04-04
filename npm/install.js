const https = require("https");
const fs = require("fs");
const path = require("path");
const os = require("os");
const { execSync } = require("child_process");

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

  // Get latest release from GitHub
  const apiUrl = `https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/latest`;
  console.log("Fetching latest release...");

  let release;
  try {
    release = await fetchJSON(apiUrl);
  } catch (err) {
    console.error(
      "Failed to fetch release info. If you are installing from source, build the binary manually."
    );
    console.error(`  Error: ${err.message}`);
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

  if (asset.name.endsWith(".tar.gz")) {
    const tmpArchive = path.join(binDir, asset.name);
    await downloadFile(asset.browser_download_url, tmpArchive);

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
    const tmpArchive = path.join(binDir, asset.name);
    await downloadFile(asset.browser_download_url, tmpArchive);

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
    // Plain binary
    await downloadFile(asset.browser_download_url, binaryPath);
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
