import { test, expect } from '@playwright/test';

// htmx boost does NOT skip links with a `download` attribute (checked: static/htmx.min.js
// has no special-casing for it) — every export/download link needs explicit hx-boost="false",
// or htmx will XHR-fetch the response and try to swap the file bytes into #mainContent as if
// it were a page. This checks the export link actually triggers a browser download, not a swap.

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
