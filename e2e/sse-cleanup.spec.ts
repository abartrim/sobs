import { test, expect } from '@playwright/test';

// logs.html's live-tail keeps an EventSource open indefinitely. Before the htmx migration
// every navigation was a full reload, so the JS realm (and any open EventSource) was
// discarded for free. Under htmx boost the realm persists across navigations, so
// window.SOBS.onElementCleanup(toggleBtn, ...) is what closes it on navigate-away — this
// test verifies that actually happens by tracking every EventSource instance the page
// constructs and asserting none are left open after leaving /logs.

test('live-tail EventSource closes on navigate-away, does not accumulate', async ({ page }) => {
  await page.addInitScript(() => {
    (window as any).__sobsE2ESources = [];
    const Native = window.EventSource;
    window.EventSource = new Proxy(Native, {
      construct(target, args) {
        const instance = new target(...(args as [string]));
        (window as any).__sobsE2ESources.push(instance);
        return instance;
      },
    }) as any;
  });

  await page.goto('/logs');
  await page.locator('#liveModeToggle').click();
  await expect(page.locator('#liveModeStatus')).not.toHaveText('Live off');

  const openCount = () =>
    page.evaluate(
      () => (window as any).__sobsE2ESources.filter((s: EventSource) => s.readyState !== EventSource.CLOSED).length
    );

  await expect.poll(openCount).toBeGreaterThan(0);

  // Navigate away (boosted) and back twice — each visit's toggle click would open a fresh
  // connection if the previous one leaked instead of being cleaned up.
  for (let i = 0; i < 2; i++) {
    await page.locator('.sidebar .nav-link[href="/traces"]').click();
    await page.waitForLoadState('networkidle');
    await expect.poll(openCount, `open EventSource count after navigating away (iteration ${i})`).toBe(0);

    await page.locator('.sidebar .nav-link[href="/logs"]').click();
    await page.waitForLoadState('networkidle');
    await page.locator('#liveModeToggle').click();
    await expect.poll(openCount).toBeGreaterThan(0);
  }
});
