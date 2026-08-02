import { test, expect } from '@playwright/test';
import { markNoFullReload, expectNoFullReload, expectSingleMainContent } from './helpers';

// The "base" fixture corpus is intentionally sparse (structural/config coverage, not volume
// — see go/testdata/fixtures/base_dump.json), so pagination isn't exercised here: there
// aren't enough seeded rows on any page to produce a second page. Sort-header clicks and
// the filter-form submit are volume-independent and covered below.

test('clicking a sort column header updates the URL and re-swaps without a full reload', async ({ page }) => {
  await page.goto('/traces');
  await expectSingleMainContent(page);

  await markNoFullReload(page);
  await page.getByRole('link', { name: /^Duration/ }).click();
  await page.waitForLoadState('networkidle');

  await expect(page).toHaveURL(/sort_by=Duration/);
  await expectSingleMainContent(page);
  await expectNoFullReload(page);

  // Clicking the same header again flips the direction.
  await markNoFullReload(page);
  await page.getByRole('link', { name: /^Duration/ }).click();
  await page.waitForLoadState('networkidle');
  await expect(page).toHaveURL(/sort_by=Duration/);
  await expectNoFullReload(page);
});

test('submitting the filter form (GET) updates the URL and re-swaps without a full reload', async ({ page }) => {
  await page.goto('/traces');
  await expectSingleMainContent(page);

  await markNoFullReload(page);
  await page.locator('.sobs-filter-form input[name="trace_id"]').fill('deadbeefdeadbeefdeadbeefdeadbeef');
  await page.locator('.sobs-filter-form button[type="submit"]').click();
  await page.waitForLoadState('networkidle');

  await expect(page).toHaveURL(/trace_id=deadbeef/);
  await expectSingleMainContent(page);
  await expectNoFullReload(page);
});
