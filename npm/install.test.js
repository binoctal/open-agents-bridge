const test = require("node:test");
const assert = require("node:assert");
const crypto = require("crypto");
const fs = require("fs");
const os = require("os");
const path = require("path");

const { verifySignature, verifyDownload, TRUSTED_KEYS } = require("./install.js");

function newKey() {
  const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
  const raw = publicKey.export({ format: "der", type: "spki" }).subarray(-32);
  return { privateKey, pub: raw.toString("base64") };
}
const sign = (priv, buf) => crypto.sign(null, buf, priv).toString("base64");

function archiveFile(content) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "oab-test-"));
  const file = path.join(dir, "asset.tar.gz");
  fs.writeFileSync(file, content);
  const sum = crypto.createHash("sha256").update(content).digest("hex");
  return { file, sum };
}

const NAME = "asset.tar.gz";

test("embedded trusted keys are well-formed", () => {
  assert.ok(TRUSTED_KEYS.length > 0);
  for (const k of TRUSTED_KEYS) assert.strictEqual(Buffer.from(k, "base64").length, 32);
});

test("valid signature and matching archive pass", () => {
  const k = newKey();
  const { file, sum } = archiveFile("genuine");
  const sums = Buffer.from(`${sum}  ${NAME}\n`);
  verifyDownload({ checksumsBuf: sums, sigText: sign(k.privateKey, sums), assetName: NAME, filePath: file, keys: [k.pub], requireSignature: true });
});

test("binary AND checksums.txt both replaced by attacker (no valid sig) => fails", () => {
  const trusted = newKey();
  const attacker = newKey();
  const { file, sum } = archiveFile("backdoored");
  const evilSums = Buffer.from(`${sum}  ${NAME}\n`); // consistent with the evil binary
  const attempts = {
    "no signature asset": null,
    "signed by attacker key": sign(attacker.privateKey, evilSums),
    "garbage signature": "not-a-signature",
    "signature over other data": sign(attacker.privateKey, Buffer.from("other")),
  };
  for (const [label, sigText] of Object.entries(attempts)) {
    assert.throws(
      () => verifyDownload({ checksumsBuf: evilSums, sigText, assetName: NAME, filePath: file, keys: [trusted.pub], requireSignature: true }),
      undefined,
      label
    );
  }
});

test("signature is checked before the checksum comparison", () => {
  const k = newKey();
  const { file } = archiveFile("x");
  const sums = Buffer.from(`${"0".repeat(64)}  ${NAME}\n`);
  assert.throws(
    () => verifyDownload({ checksumsBuf: sums, sigText: "AAAA", assetName: NAME, filePath: file, keys: [k.pub], requireSignature: true }),
    /malformed|does not verify/
  );
});

test("valid signature but tampered archive still fails the checksum", () => {
  const k = newKey();
  const { file } = archiveFile("tampered");
  const sums = Buffer.from(`${crypto.createHash("sha256").update("published").digest("hex")}  ${NAME}\n`);
  assert.throws(
    () => verifyDownload({ checksumsBuf: sums, sigText: sign(k.privateKey, sums), assetName: NAME, filePath: file, keys: [k.pub], requireSignature: true }),
    /Checksum mismatch/
  );
});

test("unsigned release allowed only when the policy is off", () => {
  const { file, sum } = archiveFile("x");
  const sums = Buffer.from(`${sum}  ${NAME}\n`);
  assert.throws(() => verifyDownload({ checksumsBuf: sums, sigText: null, assetName: NAME, filePath: file, keys: [], requireSignature: true }));
  verifyDownload({ checksumsBuf: sums, sigText: null, assetName: NAME, filePath: file, keys: [], requireSignature: false });
});

test("any key in the rotation list verifies", () => {
  const oldK = newKey();
  const newK = newKey();
  const data = Buffer.from("sums");
  verifySignature(data, sign(oldK.privateKey, data), [newK.pub, oldK.pub]);
});
