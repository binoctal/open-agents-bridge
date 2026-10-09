# Release signing

Every release carries `checksums.txt.sig`: a detached Ed25519 signature over
the exact bytes of `checksums.txt`, stored as base64. `checksums.txt` lists the
SHA256 of every archive. The updater (`internal/updater`) and the npm
installer (`npm/install.js`) verify the signature FIRST and only then compare
the archive hash, so an attacker who replaces both an archive and
`checksums.txt` on the GitHub Release still cannot get it installed.

## Key custody

- Algorithm: Ed25519. Tooling: `scripts/release-sign` (`keygen`, `pubkey`, `sign`).
- The private key is `~/.config/open-agents/release-signing/ed25519.key`
  (directory 0700, file 0600) on the maintainer's machine. It is never in the
  repository, never in CI secrets, never printed.
- Keep an offline backup (encrypted USB / password manager attachment). Loss
  of the only copy is handled by the rotation runbook below, but only with a
  working old key can rotation be seamless.
- The public keys are embedded in two places that MUST stay identical:
  `trustedKeys` in `internal/updater/signature.go` and `TRUSTED_KEYS` in
  `npm/install.js`. Both are lists so keys can rotate.

## Signing a release

1. Push the `vX.Y.Z` tag. CI (GoReleaser) builds and publishes the archives
   and `checksums.txt`. The `publish-npm` job then stops at its guard because
   `checksums.txt.sig` is missing; that is intended.
2. On the maintainer machine: `make release-sign TAG=vX.Y.Z`. This downloads
   the Release's `checksums.txt`, signs it with the offline key and uploads
   `checksums.txt.sig` to the same Release.
3. Re-run the failed `publish-npm` job (or the whole workflow) so the npm
   package is published.

Never re-sign after altering `checksums.txt` by hand: sign what is on the Release.

## Policy

`updater.requireSignature` (string, default `"true"`; override with
`-ldflags "-X .../internal/updater.requireSignature=false"`) is the
transitional switch. While true, a release without a valid signature is
refused by the updater. `REQUIRE_SIGNATURE` in `npm/install.js` is the npm
counterpart. Bridges older than the first signed release do not verify
signatures at all; they keep their SHA256-only check.

## Rotation / leak / loss runbook

Goal: move trust to a new key without ever leaving users on a key the
attacker controls.

Planned rotation or suspected leak with the OLD key still in our hands:
1. `go run ./scripts/release-sign keygen -key <new path>`; note the printed public key.
2. Add the NEW public key to both lists (keep the old one for now) and commit.
3. Release vN signed with the OLD key. Bridges and installers that trust the
   old key accept vN and now also trust the new key.
4. Wait until the install base has moved to vN or later (check the version
   share in the `[bridge-handshake]` telemetry).
5. Release vN+1 that removes the old key from both lists, signed with the NEW key.
6. Move the new key to the default key path; destroy the old key material.

If the old key leaked, an attacker can still sign a malicious release that
the old list accepts, so do steps 1 to 3 immediately, publish an advisory
telling users to update to vN (or reinstall from a verified source), and keep
step 4 short.

Total loss (no old key, nothing signable): already-deployed bridges cannot be
moved by any release. Users must reinstall from the official channel, which
ships the new key list. Publish the new key and install instructions on the
official channel list, then release with the new key.
