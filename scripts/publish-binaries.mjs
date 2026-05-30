#!/usr/bin/env node
// Copyright 2026 Zibby Lab. Apache-2.0.
//
// publish-binaries.mjs — mirror goreleaser output to dl.zibby.app.
//
// Sibling of studio/scripts/release-binary.mjs in the parent zibby tree:
// same bucket, same CloudFront distribution, same `dl.zibby.app` host,
// same AWS_PROFILE=zibby. Studio publishes under `download/`; agent-ops
// publishes under `agent-ops/`. Both layouts coexist as siblings in the
// one downloads bucket — DO NOT touch Studio's prefix.
//
// What this does
// --------------
// 1. Scans ../dist (goreleaser output dir; produced by
//    `goreleaser release --snapshot --clean` or a real `goreleaser release`).
// 2. Discovers `agent-ops_<os>_<arch>.tar.gz` + `agent-ops_<ver>_<os>_<arch>.deb`
//    artifacts. Pulls the release version out of the goreleaser metadata
//    file (`dist/metadata.json`) — falls back to parsing artifact filenames
//    if the metadata file is missing.
// 3. Uploads each artifact to `s3://<bucket>/agent-ops/v<ver>/`.
// 4. Mirrors each to `s3://<bucket>/agent-ops/latest/` (stable URLs).
// 5. Computes SHA-256 per artifact, writes `SHA256SUMS` + uploads it.
// 6. Maintains a JSON release index at `agent-ops/releases.json` (same
//    format Studio uses at `download/releases.json` so the frontend can
//    reuse the picker code if we ever surface "Previous versions").
// 7. CloudFront-invalidates the touched paths.
//
// Run it
// ------
//   node scripts/publish-binaries.mjs --dry-run     # print the plan, touch nothing
//   node scripts/publish-binaries.mjs               # publish v<X> from dist/
//   node scripts/publish-binaries.mjs --rebuild-index   # rescan S3, regenerate releases.json only
//
// Required env / defaults (mirrors Studio's release-binary.mjs)
// ------------------------------------------------------------
//   AWS_PROFILE                 defaulted to "zibby" if unset
//   AGENT_OPS_RELEASE_BUCKET    defaults to the shared Studio downloads bucket
//   AGENT_OPS_CLOUDFRONT_ID     defaults to the dl.zibby.app distribution
//
// Why this script and not goreleaser's own S3 publisher?
// ------------------------------------------------------
// goreleaser's built-in `s3` blob publisher uploads under a fixed prefix
// with the same naming, but doesn't maintain a `latest/` alias or a
// `releases.json` index — both of which the Zibby frontend (and this
// project's README install snippet) depend on. Easier to drive S3
// ourselves than fight the goreleaser blob templating.

import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const __filename = fileURLToPath(import.meta.url);
const scriptsDir = path.dirname(__filename);
const repoRoot = path.resolve(scriptsDir, '..');
const distDir = path.join(repoRoot, 'dist');

const DEFAULTS = {
  bucket: 'zibbystudiodownloadsprod-studiodownloadsbucketa7b7-xw8j7htykcp2',
  cloudFrontDistributionId: 'E2RXTLGIV1TGD9',
  baseUrl: 'https://dl.zibby.app',
  awsProfile: 'zibby',
  s3Prefix: 'agent-ops',
};

function log(msg) {
  process.stdout.write(`${msg}\n`);
}

function fail(msg) {
  process.stderr.write(`\n[publish-binaries] ${msg}\n`);
  process.exit(1);
}

function exists(p) {
  try {
    fs.accessSync(p, fs.constants.F_OK);
    return true;
  } catch {
    return false;
  }
}

function sha256File(filePath) {
  const hash = crypto.createHash('sha256');
  const buf = fs.readFileSync(filePath);
  hash.update(buf);
  return hash.digest('hex');
}

function toCloudFrontPath(key) {
  const cleanKey = String(key || '').trim().replace(/^\/+/, '');
  if (!cleanKey) return '/';
  const encoded = cleanKey.split('/').map((part) => encodeURIComponent(part)).join('/');
  return `/${encoded}`;
}

function parseBooleanLike(value) {
  const v = String(value ?? '').trim().toLowerCase();
  if (!v) return null;
  if (['1', 'true', 'yes', 'y', 'on'].includes(v)) return true;
  if (['0', 'false', 'no', 'n', 'off'].includes(v)) return false;
  return null;
}

function getCliOptions(argv) {
  const out = {
    dryRun: false,
    rebuildIndex: false,
    bucket: '',
    cloudFrontDistributionId: '',
    distDir: '',
  };
  for (let i = 0; i < argv.length; i++) {
    const a = String(argv[i] || '').trim();
    if (!a.startsWith('--')) continue;
    const [rawKey, inlineValue] = a.split('=', 2);
    const key = rawKey.replace(/^--/, '');
    const next = inlineValue ?? argv[i + 1];
    const hasValue = inlineValue !== undefined || (next && !String(next).startsWith('--'));
    switch (key) {
      case 'dry-run':
        out.dryRun = true;
        break;
      case 'rebuild-index':
        out.rebuildIndex = true;
        break;
      case 'bucket':
        out.bucket = hasValue ? String(next).trim() : '';
        if (inlineValue === undefined && hasValue) i++;
        break;
      case 'cloudfront-distribution-id':
        out.cloudFrontDistributionId = hasValue ? String(next).trim() : '';
        if (inlineValue === undefined && hasValue) i++;
        break;
      case 'dist':
        out.distDir = hasValue ? String(next).trim() : '';
        if (inlineValue === undefined && hasValue) i++;
        break;
      default:
        break;
    }
  }
  return out;
}

function runCommand(cmd, args, { capture = false } = {}) {
  return new Promise((resolve, reject) => {
    const stdio = capture ? ['ignore', 'pipe', 'pipe'] : 'inherit';
    const child = spawn(cmd, args, { cwd: repoRoot, stdio, shell: false });
    let stdout = '';
    let stderr = '';
    if (capture) {
      child.stdout.on('data', (chunk) => { stdout += chunk.toString(); });
      child.stderr.on('data', (chunk) => { stderr += chunk.toString(); });
    }
    child.on('error', reject);
    child.on('close', (code) => {
      if (code === 0) resolve(stdout);
      else reject(new Error(`${cmd} ${args.join(' ')} failed (code ${code})${capture ? `: ${stderr || stdout}` : ''}`));
    });
  });
}

// Parse the goreleaser version out of dist/metadata.json (preferred) or
// fall back to artifact filenames. Both paths exist because operators may
// run `goreleaser release --snapshot` (writes metadata.json) OR copy raw
// artifacts into dist/ from a CI build (no metadata file).
function discoverVersion(distRoot) {
  const metadataPath = path.join(distRoot, 'metadata.json');
  if (exists(metadataPath)) {
    try {
      const meta = JSON.parse(fs.readFileSync(metadataPath, 'utf8'));
      const v = String(meta?.version || '').trim();
      if (v) return { version: v, source: 'metadata.json' };
    } catch (e) {
      log(`[publish-binaries] metadata.json present but unparseable: ${e.message}`);
    }
  }
  // Fallback — peek inside dist for any `agent-ops_<ver>_<os>_<arch>.deb`.
  // goreleaser writes debs with the version embedded; tarballs don't (their
  // template is intentionally version-less so the latest/ alias points at a
  // stable name). The .deb is the easier source-of-truth here.
  if (exists(distRoot)) {
    for (const entry of fs.readdirSync(distRoot)) {
      const m = entry.match(/^agent-ops_([0-9][^_]*)_(linux|darwin)_(amd64|arm64)\.deb$/);
      if (m) return { version: m[1], source: `filename:${entry}` };
    }
  }
  return { version: '', source: '' };
}

// Tarballs are named `agent-ops_<os>_<arch>.tar.gz` (no version), debs are
// `agent-ops_<ver>_<os>_<arch>.deb`. Plus `checksums.txt` / `SHA256SUMS.txt`
// from goreleaser. We upload tarballs + debs only; the goreleaser checksum
// file is GH-Release-specific and we generate our own SHA256SUMS from the
// uploaded set so users can verify a single dl.zibby.app download.
function discoverArtifacts(distRoot) {
  if (!exists(distRoot)) return [];
  const out = [];
  for (const entry of fs.readdirSync(distRoot)) {
    const full = path.join(distRoot, entry);
    if (!fs.statSync(full).isFile()) continue;
    if (/^agent-ops_(linux|darwin)_(amd64|arm64)\.tar\.gz$/.test(entry)) {
      out.push(full);
      continue;
    }
    if (/^agent-ops_[0-9][^_]*_(linux|darwin)_(amd64|arm64)\.deb$/.test(entry)) {
      out.push(full);
      continue;
    }
  }
  return out.sort();
}

async function checkAwsCli({ dryRun }) {
  if (dryRun) return;
  try {
    await runCommand('aws', ['--version'], { capture: true });
  } catch {
    fail('AWS CLI not found. Install/configure aws cli (or pass --dry-run to skip uploads).');
  }
}

async function awsCp(localPath, s3Url, { dryRun }) {
  if (dryRun) {
    log(`  [dry-run] aws s3 cp ${path.relative(repoRoot, localPath) || localPath} ${s3Url}`);
    return;
  }
  await runCommand('aws', ['s3', 'cp', localPath, s3Url, '--no-progress']);
}

async function awsCopyS3(srcS3Url, dstS3Url, { dryRun }) {
  if (dryRun) {
    log(`  [dry-run] aws s3 cp ${srcS3Url} ${dstS3Url}`);
    return;
  }
  await runCommand('aws', ['s3', 'cp', srcS3Url, dstS3Url, '--no-progress']);
}

async function cloudFrontInvalidate({ distributionId, paths, dryRun }) {
  if (!distributionId) {
    log('Skipping CloudFront invalidation (no distribution id).');
    return;
  }
  if (paths.length === 0) return;
  const maxPathsPerInvalidation = 900;
  for (let i = 0; i < paths.length; i += maxPathsPerInvalidation) {
    const batch = paths.slice(i, i + maxPathsPerInvalidation);
    if (dryRun) {
      log(`  [dry-run] aws cloudfront create-invalidation --distribution-id ${distributionId} --paths ${batch.join(' ')}`);
      continue;
    }
    await runCommand('aws', [
      'cloudfront', 'create-invalidation',
      '--distribution-id', distributionId,
      '--paths', ...batch,
    ]);
  }
}

// Idempotent rebuild of agent-ops/releases.json from S3 state alone — no
// build, no publish. Mirrors studio/scripts/release-binary.mjs's
// --rebuild-index path. Useful when the index drifts from the bucket (manual
// yanks, first-time bootstrap, etc.).
async function rebuildReleasesIndex({ bucket, cloudFrontDistributionId, dryRun }) {
  log(`\n=== Rebuilding releases index from s3://${bucket}/${DEFAULTS.s3Prefix}/ ===\n`);
  const lsOutput = await runCommand('aws', ['s3', 'ls', `s3://${bucket}/${DEFAULTS.s3Prefix}/`], { capture: true });
  const versionDirs = lsOutput
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line.startsWith('PRE '))
    .map((line) => line.replace(/^PRE\s+/, '').replace(/\/$/, ''))
    .filter((name) => /^v\d+\.\d+\.\d+/.test(name));
  if (versionDirs.length === 0) {
    fail(`No ${DEFAULTS.s3Prefix}/v*/ folders found at s3://${bucket}/${DEFAULTS.s3Prefix}/.`);
  }
  log(`Found ${versionDirs.length} version folder(s): ${versionDirs.join(', ')}`);

  const tmpDir = path.join(distDir, 'rebuild-index-tmp');
  fs.mkdirSync(tmpDir, { recursive: true });

  const releases = [];
  for (const versionDir of versionDirs) {
    const version = versionDir.replace(/^v/, '');
    const versionPrefix = `${DEFAULTS.s3Prefix}/${versionDir}`;
    const manifestKey = `${versionPrefix}/manifest.json`;
    const manifestLocal = path.join(tmpDir, `${versionDir}-manifest.json`);
    let manifest = null;
    try {
      await runCommand('aws', ['s3', 'cp', `s3://${bucket}/${manifestKey}`, manifestLocal, '--no-progress'], { capture: true });
      manifest = JSON.parse(fs.readFileSync(manifestLocal, 'utf8'));
    } catch {
      // No manifest — synthesize from file listing.
    }
    let entry;
    if (manifest && Array.isArray(manifest.artifacts)) {
      entry = {
        version,
        releasedAt: manifest.generatedAt || null,
        manifestKey,
        artifacts: manifest.artifacts.map((a) => ({
          file: a.file,
          versionedKey: a.versionedKey || `${versionPrefix}/${a.file}`,
          sha256: a.sha256 || null,
        })),
      };
    } else {
      const filesOutput = await runCommand('aws', ['s3', 'ls', `s3://${bucket}/${versionPrefix}/`], { capture: true });
      const files = [];
      let oldestModified = null;
      for (const rawLine of filesOutput.split('\n')) {
        const line = rawLine.trim();
        if (!line || line.startsWith('PRE ')) continue;
        const match = line.match(/^(\S+\s+\S+)\s+\d+\s+(.+)$/);
        if (!match) continue;
        const [, modifiedStr, filename] = match;
        if (filename === 'manifest.json' || filename === 'SHA256SUMS') continue;
        files.push(filename);
        const mod = new Date(modifiedStr);
        if (!oldestModified || mod < oldestModified) oldestModified = mod;
      }
      if (files.length === 0) continue;
      entry = {
        version,
        releasedAt: oldestModified ? oldestModified.toISOString() : null,
        manifestKey,
        artifacts: files.map((file) => ({
          file,
          versionedKey: `${versionPrefix}/${file}`,
          sha256: null,
        })),
      };
    }
    releases.push(entry);
  }

  releases.sort((a, b) => {
    const pa = String(a.version).split('.').map((n) => parseInt(n, 10) || 0);
    const pb = String(b.version).split('.').map((n) => parseInt(n, 10) || 0);
    for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
      const da = pa[i] ?? 0;
      const db = pb[i] ?? 0;
      if (da !== db) return db - da;
    }
    return 0;
  });

  const newIndex = {
    generatedAt: new Date().toISOString(),
    latestVersion: releases[0]?.version || null,
    releases,
  };
  const indexLocal = path.join(tmpDir, 'releases.json');
  fs.writeFileSync(indexLocal, `${JSON.stringify(newIndex, null, 2)}\n`, 'utf8');

  const indexKey = `${DEFAULTS.s3Prefix}/releases.json`;
  log(`\nUploading rebuilt index to s3://${bucket}/${indexKey} ...`);
  await awsCp(indexLocal, `s3://${bucket}/${indexKey}`, { dryRun });
  log(`Index now lists ${newIndex.releases.length} release(s); newest=${newIndex.latestVersion}.`);

  await cloudFrontInvalidate({
    distributionId: cloudFrontDistributionId,
    paths: [toCloudFrontPath(indexKey)],
    dryRun,
  });
  try { fs.rmSync(tmpDir, { recursive: true, force: true }); } catch { /* ignore */ }
  log('\nDone.');
}

async function main() {
  if (!process.env.AWS_PROFILE) {
    process.env.AWS_PROFILE = DEFAULTS.awsProfile;
  }
  const cli = getCliOptions(process.argv.slice(2));
  const bucket = cli.bucket || process.env.AGENT_OPS_RELEASE_BUCKET || DEFAULTS.bucket;
  const cloudFrontDistributionId =
    cli.cloudFrontDistributionId
    || process.env.AGENT_OPS_CLOUDFRONT_ID
    || DEFAULTS.cloudFrontDistributionId;
  const distRoot = cli.distDir ? path.resolve(cli.distDir) : distDir;
  const dryRun = cli.dryRun;

  log('\n=== agent-ops binary publish ===');
  log(`Mode:        ${dryRun ? 'dry-run (no S3/CloudFront mutations)' : 'live'}`);
  log(`Bucket:      ${bucket}`);
  log(`Prefix:      ${DEFAULTS.s3Prefix}/`);
  log(`CloudFront:  ${cloudFrontDistributionId || '(none)'}`);
  log(`Dist dir:    ${distRoot}`);

  await checkAwsCli({ dryRun });

  if (cli.rebuildIndex) {
    await rebuildReleasesIndex({ bucket, cloudFrontDistributionId, dryRun });
    return;
  }

  if (!exists(distRoot)) {
    fail(`Dist dir not found: ${distRoot}. Run \`goreleaser release --snapshot --clean\` first.`);
  }

  const { version, source: versionSource } = discoverVersion(distRoot);
  if (!version) {
    fail('Could not determine release version (no dist/metadata.json and no agent-ops_*_*.deb in dist/).');
  }
  log(`Version:     ${version}  (source: ${versionSource})`);

  const artifacts = discoverArtifacts(distRoot);
  if (artifacts.length === 0) {
    fail(`No agent-ops artifacts found in ${distRoot}.`);
  }

  // Compute SHAs + write SHA256SUMS into a temp staging dir so we don't
  // pollute the goreleaser dist tree.
  const stagingDir = path.join(distRoot, 'publish-staging');
  fs.mkdirSync(stagingDir, { recursive: true });
  const shaByFile = new Map();
  const shaLines = [];
  for (const filePath of artifacts) {
    const sha = sha256File(filePath);
    const base = path.basename(filePath);
    shaByFile.set(base, sha);
    shaLines.push(`${sha}  ${base}`);
  }
  const shaPath = path.join(stagingDir, 'SHA256SUMS');
  fs.writeFileSync(shaPath, `${shaLines.join('\n')}\n`, 'utf8');

  log('\nArtifacts:');
  for (const filePath of artifacts) {
    const sizeMb = (fs.statSync(filePath).size / (1024 * 1024)).toFixed(2);
    log(`  - ${path.basename(filePath)} (${sizeMb} MB)`);
  }
  log(`  - SHA256SUMS`);

  // 1. Upload all artifacts to agent-ops/v<ver>/.
  const versionPrefix = `${DEFAULTS.s3Prefix}/v${version}`;
  const latestPrefix = `${DEFAULTS.s3Prefix}/latest`;
  const invalidationKeys = new Set();
  const s3VersionBase = `s3://${bucket}/${versionPrefix}/`;
  const s3LatestBase = `s3://${bucket}/${latestPrefix}/`;

  log(`\nUploading to ${s3VersionBase} ...`);
  for (const filePath of [...artifacts, shaPath]) {
    const base = path.basename(filePath);
    log(`  -> ${base}`);
    await awsCp(filePath, `${s3VersionBase}${base}`, { dryRun });
    invalidationKeys.add(`${versionPrefix}/${base}`);
  }

  // 2. Mirror to agent-ops/latest/. README install snippet curls from here.
  log(`\nUpdating latest aliases at ${s3LatestBase} ...`);
  for (const filePath of [...artifacts, shaPath]) {
    const base = path.basename(filePath);
    // For debs we keep the versioned filename in latest/ too so users can
    // distinguish, but we ALSO publish a stable-name copy with the version
    // stripped (Studio does the same thing for `Zibby Studio-<ver>-….dmg`).
    await awsCopyS3(`${s3VersionBase}${base}`, `${s3LatestBase}${base}`, { dryRun });
    invalidationKeys.add(`${latestPrefix}/${base}`);

    const stableName = base.replace(`_${version}_`, '_');
    if (stableName !== base) {
      await awsCopyS3(`${s3VersionBase}${base}`, `${s3LatestBase}${stableName}`, { dryRun });
      invalidationKeys.add(`${latestPrefix}/${stableName}`);
    }
  }

  // 3. Write + upload manifest.json (per-version + latest).
  const manifest = {
    version,
    generatedAt: new Date().toISOString(),
    versionPrefix,
    latestPrefix,
    artifacts: artifacts.map((filePath) => {
      const base = path.basename(filePath);
      return {
        file: base,
        sha256: shaByFile.get(base),
        versionedKey: `${versionPrefix}/${base}`,
        latestKey: `${latestPrefix}/${base}`,
      };
    }),
  };
  const manifestLocal = path.join(stagingDir, 'manifest.json');
  fs.writeFileSync(manifestLocal, `${JSON.stringify(manifest, null, 2)}\n`, 'utf8');

  log(`\nUploading manifest.json to ${s3VersionBase}manifest.json ...`);
  await awsCp(manifestLocal, `${s3VersionBase}manifest.json`, { dryRun });
  invalidationKeys.add(`${versionPrefix}/manifest.json`);
  log(`Uploading manifest.json to ${s3LatestBase}manifest.json ...`);
  await awsCp(manifestLocal, `${s3LatestBase}manifest.json`, { dryRun });
  invalidationKeys.add(`${latestPrefix}/manifest.json`);

  // 4. Maintain agent-ops/releases.json — read existing, upsert this version,
  //    sort newest-first by semver, write back. Same upsert semantics as
  //    Studio's release-binary.mjs.
  const releasesIndexKey = `${DEFAULTS.s3Prefix}/releases.json`;
  const releasesIndexS3 = `s3://${bucket}/${releasesIndexKey}`;
  const releasesIndexLocal = path.join(stagingDir, 'releases.json');
  let existingIndex = { releases: [] };
  if (!dryRun) {
    try {
      await runCommand('aws', ['s3', 'cp', releasesIndexS3, releasesIndexLocal, '--no-progress'], { capture: true });
      existingIndex = JSON.parse(fs.readFileSync(releasesIndexLocal, 'utf8'));
      if (!Array.isArray(existingIndex.releases)) existingIndex.releases = [];
    } catch {
      log(`No existing ${releasesIndexKey} — creating.`);
      existingIndex = { releases: [] };
    }
  } else {
    log(`[dry-run] would read existing ${releasesIndexS3}; assuming empty for plan output.`);
  }

  const newEntry = {
    version,
    releasedAt: manifest.generatedAt,
    manifestKey: `${versionPrefix}/manifest.json`,
    artifacts: manifest.artifacts.map((a) => ({
      file: a.file,
      versionedKey: a.versionedKey,
      sha256: a.sha256,
    })),
  };
  const filtered = existingIndex.releases.filter((r) => r.version !== version);
  filtered.push(newEntry);
  filtered.sort((a, b) => {
    const pa = String(a.version).split('.').map((n) => parseInt(n, 10) || 0);
    const pb = String(b.version).split('.').map((n) => parseInt(n, 10) || 0);
    for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
      const da = pa[i] ?? 0;
      const db = pb[i] ?? 0;
      if (da !== db) return db - da;
    }
    return 0;
  });
  const newIndex = {
    generatedAt: manifest.generatedAt,
    latestVersion: filtered[0]?.version || version,
    releases: filtered,
  };
  fs.writeFileSync(releasesIndexLocal, `${JSON.stringify(newIndex, null, 2)}\n`, 'utf8');
  log(`\nUpdating releases index at ${releasesIndexS3} (${newIndex.releases.length} version(s)) ...`);
  await awsCp(releasesIndexLocal, releasesIndexS3, { dryRun });
  invalidationKeys.add(releasesIndexKey);

  // 5. CloudFront invalidate. Use granular paths so other prefixes
  //    (e.g. Studio's `download/`) aren't disturbed.
  await cloudFrontInvalidate({
    distributionId: cloudFrontDistributionId,
    paths: Array.from(invalidationKeys, (k) => toCloudFrontPath(k)),
    dryRun,
  });

  log('\nDone.');
  log('Public URL patterns:');
  log(`  Versioned:  ${DEFAULTS.baseUrl}/${versionPrefix}/<artifact>`);
  log(`  Latest:     ${DEFAULTS.baseUrl}/${latestPrefix}/<artifact>`);
  log(`  Index:      ${DEFAULTS.baseUrl}/${releasesIndexKey}`);
}

main().catch((e) => fail(e?.message || String(e)));
