#!/usr/bin/env node
/**
 * build-rum.js – build script for static/rum.js
 *
 * Modes:
 *   node scripts/build-rum.js dev   – generate source map for the readable rum.js
 *   node scripts/build-rum.js prod  – minified JS + source map (rum.min.js + rum.min.js.map)
 *   node scripts/build-rum.js       – runs both dev and prod
 *
 * Outputs:
 *   static/rum.js         – unchanged readable source (not modified by this script)
 *   static/rum.js.map     – dev source map; served via X-SourceMap HTTP header
 *   static/rum.min.js     – minified production bundle
 *   static/rum.min.js.map – production source map (includes original sources)
 *
 * Cache busting:
 *   The prod build appends a content-hash query hint in a comment inside rum.min.js.
 *   The Flask routes use ETags (file content hash) for deterministic HTTP cache busting.
 */

'use strict';

const fs = require('fs');
const path = require('path');
const crypto = require('crypto');
const { minify } = require('terser');

const ROOT = path.resolve(__dirname, '..');
const SRC = path.join(ROOT, 'static', 'rum.js');
const OUT_DEV_MAP = path.join(ROOT, 'static', 'rum.js.map');
const OUT_PROD = path.join(ROOT, 'static', 'rum.min.js');
const OUT_PROD_MAP = path.join(ROOT, 'static', 'rum.min.js.map');

function contentHash(buf) {
  return crypto.createHash('sha256').update(buf).digest('hex').slice(0, 8);
}

/**
 * Dev build: generate rum.js.map without modifying rum.js.
 * The source map is served via the X-SourceMap HTTP response header so that
 * browser DevTools can load it automatically without an inline comment.
 */
async function buildDev(src) {
  console.log('[rum:dev] generating source map for rum.js …');
  const result = await minify(
    { 'rum.js': src },
    {
      compress: false,
      mangle: false,
      format: {
        comments: 'all',
        semicolons: true,
      },
      sourceMap: {
        filename: 'rum.js',
        // No url here – we don't want a sourceMappingURL comment in rum.js;
        // the map is served via the X-SourceMap HTTP header instead.
        includeSources: true,
      },
    }
  );

  fs.writeFileSync(OUT_DEV_MAP, result.map, 'utf8');

  const mapSize = (fs.statSync(OUT_DEV_MAP).size / 1024).toFixed(1);
  console.log(`[rum:dev] wrote rum.js.map (${mapSize} KB)`);
  console.log('[rum:dev] rum.js is unchanged; map is served via X-SourceMap header.');
}

/**
 * Prod build: minify rum.js → rum.min.js with full source map.
 * The source map includes original sources so rum.min.js is fully debuggable.
 */
async function buildProd(src) {
  console.log('[rum:prod] minifying rum.js → rum.min.js …');
  const result = await minify(
    { 'rum.js': src },
    {
      compress: {
        drop_console: false,
        passes: 2,
      },
      mangle: true,
      format: {
        comments: false,
      },
      sourceMap: {
        filename: 'rum.min.js',
        url: 'rum.min.js.map',
        includeSources: true,
      },
    }
  );

  // Prepend a banner with the content hash for deterministic cache busting.
  const hash = contentHash(Buffer.from(result.code, 'utf8'));
  const banner = `/* sobs-rum v${hash} */\n`;
  const finalCode = banner + result.code;

  fs.writeFileSync(OUT_PROD, finalCode, 'utf8');
  fs.writeFileSync(OUT_PROD_MAP, result.map, 'utf8');

  const origSize = Buffer.byteLength(src, 'utf8');
  const minSize = Buffer.byteLength(finalCode, 'utf8');
  const pct = ((1 - minSize / origSize) * 100).toFixed(1);
  console.log(
    `[rum:prod] wrote rum.min.js (${minSize} bytes, ${pct}% smaller than source) + rum.min.js.map`
  );
  console.log(`[rum:prod] content hash: ${hash}`);
}

async function main() {
  const mode = process.argv[2] || 'all';
  if (!['dev', 'prod', 'all'].includes(mode)) {
    console.error(`Unknown mode: ${mode}. Use dev, prod, or omit for both.`);
    process.exit(1);
  }

  const src = fs.readFileSync(SRC, 'utf8');

  if (mode === 'dev' || mode === 'all') await buildDev(src);
  if (mode === 'prod' || mode === 'all') await buildProd(src);

  console.log('[rum] build complete.');
}

main().catch((err) => {
  console.error('[rum] build failed:', err);
  process.exit(1);
});

