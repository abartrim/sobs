import { test, expect } from '@playwright/test';

// A boosted request whose response isn't 2xx or has no #mainContent to select (a disabled
// feature, a template error, a 404) must fall back to a real navigation instead of quietly
// leaving #mainContent blank — see base.html's htmx:beforeSwap listener. /kubernetes is a
// real always-available example: it's disabled by default (returns a plain-text 404, not a
// full #mainContent-bearing page) and, being disabled, has no sidebar link to click, so the
// boosted request is issued directly via htmx.ajax — the exact same call hx-boost makes
// internally for a real link click.

test('a boosted request to a disabled route falls back to a real navigation, not a blank page', async ({
  page,
}) => {
  await page.goto('/logs');
  await expect(page.locator('#mainContent')).toHaveCount(1);

  await page.evaluate(() => {
    (window as any).htmx.ajax('GET', '/kubernetes', {
      target: '#mainContent',
      select: '#mainContent',
      swap: 'outerHTML',
    });
  });

  await page.waitForURL(/\/kubernetes$/);
  await expect(page.locator('body')).toContainText(/disabled/i);
});
