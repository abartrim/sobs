import { test, expect } from '@playwright/test';

// Regression test for a real bug found while building the htmx shell: the cron builder's
// init function used to early-return via a `window.__dmCronBuilderInit` flag meant to stop
// duplicate listener registration within one page load. Under htmx boost that flag stuck
// forever (window persists across navigations), so the SECOND boosted visit to this page
// silently skipped binding the listeners entirely — changing the preset dropdown did
// nothing. The fix moved the guard onto a DOM dataset attribute on the (freshly rendered
// every visit) preset element instead of `window`. This test visits the page twice via
// boosted nav and checks the builder is still interactive on the second visit.

test('the cron builder stays interactive after a second boosted visit', async ({ page }) => {
  // Goes straight to /settings/data-management rather than through the bare /settings index
  // (not what's under test here, and not needed to reach this page).
  await page.goto('/settings/data-management');

  const preset = page.locator('#cronPreset');
  const preview = page.locator('#cronPreview');

  await preset.selectOption('hourly');
  const firstVisitPreview = await preview.textContent();
  expect(firstVisitPreview).toBeTruthy();

  // Leave (any other boosted page) and come back — the same JS realm handles this second
  // render. The sidebar only links to the bare /settings index (not this sub-page directly),
  // so the return hop goes through htmx.ajax() — the same call hx-boost makes internally for
  // a real link click — rather than a visible link.
  await page.locator('.sidebar .nav-link[href="/logs"]').click();
  await page.waitForLoadState('networkidle');
  await page.evaluate(() => {
    (window as any).htmx.ajax('GET', '/settings/data-management', {
      target: '#mainContent',
      select: '#mainContent',
      swap: 'outerHTML',
    });
  });
  await page.waitForURL(/\/settings\/data-management$/);
  await page.waitForLoadState('networkidle');

  await preset.selectOption('every6h');
  await expect(preview).not.toHaveText(firstVisitPreview ?? '');
  await expect(preview).not.toBeEmpty();
});
