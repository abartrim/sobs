import { Page, expect } from '@playwright/test';

/**
 * Sets a marker on `window` that a boosted (client-side) navigation preserves but a full
 * page reload wipes. Call before triggering a nav, then assert with `expectNoFullReload`.
 */
export async function markNoFullReload(page: Page): Promise<void> {
  await page.evaluate(() => {
    (window as any).__sobsE2ENavMarker = 'still-here';
  });
}

export async function expectNoFullReload(page: Page): Promise<void> {
  const marker = await page.evaluate(() => (window as any).__sobsE2ENavMarker);
  expect(marker, 'expected a boosted swap, but the page did a full reload').toBe('still-here');
}

/** Exactly one #mainContent node exists — the core hazard the outerHTML swap design guards against. */
export async function expectSingleMainContent(page: Page): Promise<void> {
  await expect(page.locator('#mainContent')).toHaveCount(1);
}

/** The sidebar nav link for `path` carries the "active" highlight. */
export async function expectActiveNavLink(page: Page, href: string): Promise<void> {
  await expect(page.locator(`.sidebar .nav-link[href="${href}"]`)).toHaveClass(/active/);
}
