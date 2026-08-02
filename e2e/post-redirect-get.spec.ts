import { test, expect } from '@playwright/test';
import { expectSingleMainContent, markNoFullReload, expectNoFullReload } from './helpers';

// Submitting a boosted POST form should stay client-side: htmx issues the POST via XHR/fetch,
// the browser follows the server's redirect (handleViewEnrichmentSettings does a real 3xx
// flashRedirect back to the same page — see go/cmd/sobs/handlers_pages.go) transparently at
// the network layer, and htmx swaps #mainContent from the final response — never a second,
// separate top-level navigation/full reload.
//
// This does NOT assert on the number of underlying HTTP requests: a real POST-redirect-GET
// legitimately produces two network round trips (POST 303, then the redirected GET) even when
// followed automatically by the browser inside one logical XHR call — Playwright's
// `page.on('request')` sees both. The actually meaningful signal for "did htmx handle this
// without a hard navigation" is markNoFullReload/expectNoFullReload, same as every other spec
// in this suite.
//
// Note: this intentionally does not assert the post-redirect flash message text. Flash
// rendering after a POST-redirect-GET has a known, pre-existing, unrelated gap (renderPage
// doesn't surface a session-stashed flash the way renderPageFlash does) tracked separately
// from this htmx migration — asserting it here would fail on a bug this suite isn't meant
// to gate.
test('a boosted settings POST redirects and swaps without a full reload', async ({ page }) => {
  await page.goto('/settings/enrichment');
  await expectSingleMainContent(page);

  const requests: string[] = [];
  page.on('request', (req) => {
    if (req.resourceType() === 'document' || req.resourceType() === 'xhr' || req.resourceType() === 'fetch') {
      requests.push(`${req.method()} ${new URL(req.url()).pathname}`);
    }
  });

  await markNoFullReload(page);
  await page.locator('form[action*="enrichment"] button[type="submit"]').click();
  await page.waitForLoadState('networkidle');

  await expect(page).toHaveURL(/\/settings\/enrichment$/);
  await expectSingleMainContent(page);
  await expectNoFullReload(page);

  // Both round trips (the POST and the redirect-followed GET) should target this same page —
  // catches an htmx misconfiguration that bounced the request somewhere unexpected.
  const relevant = requests.filter((r) => r.includes('/settings/enrichment'));
  expect(relevant.length, `saw: ${requests.join(', ')}`).toBeGreaterThan(0);
  expect(relevant[0]).toMatch(/^POST /);
});
