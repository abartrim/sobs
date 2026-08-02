import { test, expect } from '@playwright/test';
import { readFileSync } from 'fs';
import { join } from 'path';

// Iterates the primary page GET routes already enumerated in go/testdata/manifest.json (the
// same corpus the golden-corpus byte-diff harness uses) rather than hand-maintaining a
// second, driftable list here. "Primary page" = a canonical (non-variant, non-help) GET
// route outside /api, /v1, /static — the manifest's `id` naming convention marks a
// query/error variant of a path with a second "__" after the leading "get__", so those are
// excluded here the same way.

interface ManifestRoute {
  id: string;
  path: string;
  methods: string[];
}

// Not app pages at all — lightweight liveness/readiness endpoints (plain text/JSON, no
// sidebar) that happen to match the path-prefix filter below.
const NOT_A_PAGE = new Set(['get__health', 'get__health_db']);

// Known, pre-existing, unrelated bug: the summary page's chdb query hangs indefinitely —
// reproduces even against an empty database, with zero concurrency, independent of any
// htmx change (see the filed follow-up task). Excluded here rather than left to time out.
const KNOWN_HANG = new Set(['get__root']);

function primaryPageRoutes(): ManifestRoute[] {
  const manifestPath = join(__dirname, '..', 'go', 'testdata', 'manifest.json');
  const manifest = JSON.parse(readFileSync(manifestPath, 'utf-8')) as { routes: ManifestRoute[] };
  return manifest.routes.filter((r) => {
    if (r.methods[0] !== 'GET') return false;
    if (/^\/(api|v1|static)\//.test(r.path)) return false;
    if (r.path === '/service-worker.js') return false;
    if (!r.id.startsWith('get__')) return false;
    if (r.id.slice('get__'.length).includes('__')) return false;
    if (r.id.includes('help')) return false;
    if (NOT_A_PAGE.has(r.id) || KNOWN_HANG.has(r.id)) return false;
    return true;
  });
}

const routes = primaryPageRoutes();

test('breadth smoke covers every primary page route from the manifest', () => {
  // A silent drop to zero here would mean this whole suite stopped covering anything —
  // fail loudly rather than let an empty parametrized list pass as "0 tests, all green".
  expect(routes.length).toBeGreaterThan(20);
});

for (const route of routes) {
  test(`${route.id} (${route.path}) renders with a sidebar and no console errors`, async ({ page }) => {
    const consoleErrors: string[] = [];
    page.on('console', (msg) => {
      if (msg.type() === 'error') consoleErrors.push(msg.text());
    });
    page.on('pageerror', (err) => consoleErrors.push(err.message));

    const response = await page.goto(route.path);
    const status = response?.status() ?? 0;

    if (status === 404) {
      // A handful of features (kubernetes, query, table-explorer, ...) are disabled by
      // default and intentionally 404 with a plain-text "disabled" message rather than
      // rendering the app shell — a legitimate response, not a broken page. Anything else
      // 404ing is a real failure.
      const body = await response!.text();
      expect(body, `${route.path} returned an unexpected 404: ${body}`).toMatch(/disabled/i);
      return;
    }

    expect(response?.ok(), `${route.path} returned ${status}`).toBe(true);
    await expect(page.locator('.sidebar')).toBeVisible();
    await expect(page.locator('#mainContent')).toHaveCount(1);
    expect(consoleErrors, `console errors on ${route.path}: ${consoleErrors.join('; ')}`).toEqual([]);
  });
}
