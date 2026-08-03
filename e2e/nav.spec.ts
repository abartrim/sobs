import { test, expect } from '@playwright/test';
import { markNoFullReload, expectNoFullReload, expectSingleMainContent, expectActiveNavLink } from './helpers';

// Clicking through the sidebar should swap #mainContent via htmx boost, never do a full
// page reload, and keep the sidebar's "active" highlight and <title> in sync with the URL.
const PAGES = [
  { href: '/', title: 'Summary' },
  { href: '/logs', title: 'Logs' },
  { href: '/traces', title: 'Traces' },
  { href: '/errors', title: 'Errors' },
  { href: '/metrics', title: 'Metrics' },
  { href: '/rum', title: 'RUM' },
  { href: '/web-traffic', title: 'Web Traffic' },
  { href: '/reports', title: 'Reports' },
  { href: '/settings', title: 'Settings' },
];

test('clicking through the sidebar boosts every hop, never a full reload', async ({ page }) => {
  await page.goto('/logs');
  await expectSingleMainContent(page);

  for (const { href, title } of PAGES) {
    await markNoFullReload(page);
    await page.locator(`.sidebar .nav-link[href="${href}"]`).click();
    await page.waitForLoadState('networkidle');

    await expect(page).toHaveURL(new RegExp(href.replace(/\//g, '\\/') + '$'));
    await expect(page).toHaveTitle(new RegExp(title));
    await expectSingleMainContent(page);
    await expectActiveNavLink(page, href);
    await expectNoFullReload(page);
  }
});

test('browser back/forward works across boosted navigations', async ({ page }) => {
  await page.goto('/logs');
  await expect(page).toHaveURL(/\/logs$/);

  await page.locator('.sidebar .nav-link[href="/traces"]').click();
  await page.waitForLoadState('networkidle');
  await expect(page).toHaveURL(/\/traces$/);

  await markNoFullReload(page);
  await page.goBack();
  await page.waitForLoadState('networkidle');
  await expect(page).toHaveURL(/\/logs$/);
  await expectSingleMainContent(page);
  await expectNoFullReload(page);

  await page.goForward();
  await page.waitForLoadState('networkidle');
  await expect(page).toHaveURL(/\/traces$/);
  await expectSingleMainContent(page);
});
