#!/usr/bin/env node
/**
 * prune-qa-screenshots.mjs
 *
 * Removes old playwright-toast-qa-* artifact directories from tests/screenshots/,
 * keeping only the N most recent runs (default: 3, configurable via QA_KEEP_RUNS).
 *
 * Usage:
 *   node scripts/prune-qa-screenshots.mjs
 *   QA_KEEP_RUNS=5 node scripts/prune-qa-screenshots.mjs
 */

import fs from 'node:fs';
import path from 'node:path';

const screenshotsDir = path.join('tests', 'screenshots');
const keepRuns = Math.max(1, Number(process.env.QA_KEEP_RUNS || 3));

if (!fs.existsSync(screenshotsDir)) {
  console.log(`No screenshots directory found at ${screenshotsDir}; nothing to prune.`);
  process.exit(0);
}

const entries = fs.readdirSync(screenshotsDir, { withFileTypes: true });
const runDirs = entries
  .filter((e) => e.isDirectory() && /^playwright-toast-qa-\d{14}$/.test(e.name))
  .map((e) => e.name)
  .sort(); // ISO-timestamp suffix sorts correctly lexicographically

const toDelete = runDirs.slice(0, Math.max(0, runDirs.length - keepRuns));

if (toDelete.length === 0) {
  console.log(`Found ${runDirs.length} run(s); nothing to prune (keeping last ${keepRuns}).`);
  process.exit(0);
}

console.log(`Found ${runDirs.length} run(s); keeping last ${keepRuns}, pruning ${toDelete.length}:`);
for (const dirName of toDelete) {
  const fullPath = path.join(screenshotsDir, dirName);
  fs.rmSync(fullPath, { recursive: true, force: true });
  console.log(`  Deleted ${fullPath}`);
}
console.log('Done.');
