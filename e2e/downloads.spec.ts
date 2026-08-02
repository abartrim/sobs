import { test, expect } from '@playwright/test';

// htmx's boost automatically skips links with a `download` attribute, but links that
// trigger a download purely via a `Content-Disposition: attachment` response header (no
// `download` attribute) need explicit hx-boost="false" — otherwise htmx would XHR-fetch the
// response and try to swap the file bytes into #mainContent as if it were a page. This
// checks the export link actually triggers a browser download, not a DOM swap.

test('the reports export link triggers a real download, not a page swap', async ({ page }) => {
  await page.goto('/reports');
  await expect(page.locator('#mainContent')).toHaveCount(1);

  const [download] = await Promise.all([
    page.waitForEvent('download'),
    page.locator('#export-all-btn').click(),
  ]);

  expect(download.suggestedFilename()).toMatch(/\.json$/);

  // The page itself must be untouched — a swap-instead-of-download would have replaced
  // #mainContent with the raw JSON bytes.
  await expect(page.locator('#mainContent')).toHaveCount(1);
  await expect(page.getByRole('heading', { name: 'Saved Reports' })).toBeVisible();
});
