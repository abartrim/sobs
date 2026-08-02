import { test, expect } from '@playwright/test';
import { expectSingleMainContent } from './helpers';

// Submitting a boosted POST form should produce exactly ONE round trip: htmx issues the
// POST, follows the server's redirect itself, and swaps #mainContent from the redirect
// target's response — never a second, separate client-side navigation.
//
// Note: this intentionally does not assert the post-redirect flash message text. Flash
// rendering after a POST-redirect-GET has a known, pre-existing, unrelated gap (renderPage
// doesn't surface a session-stashed flash the way renderPageFlash does) tracked separately
// from this htmx migration — asserting it here would fail on a bug this suite isn't meant
// to gate.
test('a boosted settings POST redirects and swaps in one round trip', async ({ page }) => {
  await page.goto('/settings/enrichment');
  await expectSingleMainContent(page);

  const requests: string[] = [];
  page.on('request', (req) => {
    if (req.resourceType() === 'document' || req.resourceType() === 'xhr' || req.resourceType() === 'fetch') {
      requests.push(`${req.method()} ${new URL(req.url()).pathname}`);
    }
  });

  await page.locator('form[action*="enrichment"] button[type="submit"]').click();
  await page.waitForLoadState('networkidle');

  await expect(page).toHaveURL(/\/settings\/enrichment$/);
  await expectSingleMainContent(page);

  const relevant = requests.filter((r) => r.includes('/settings/enrichment'));
  expect(relevant, `saw: ${relevant.join(', ')}`).toHaveLength(1);
  expect(relevant[0]).toMatch(/^POST /);
});
