import { test, expect } from '@playwright/test';

// ECharts instances (and their window resize listeners) used to leak across boosted
// revisits before window.SOBS.onElementCleanup(chartEl, ...) was wired in — each revisit
// would leave the previous instance's canvas and resize listener alive. Visiting a chart
// page repeatedly should always leave exactly one canvas in the chart container, never a
// growing pile of stale ones.

test('revisiting a chart page via boosted nav repeatedly leaves exactly one canvas', async ({ page }) => {
  // A hard page.goto() would reload the JS realm from scratch every time regardless of any
  // leak, masking the bug this guards against — the first visit is a real navigation, but
  // every repeat visit goes through the page's own "clear filters" link (which points back
  // at the same bare /metrics/anomaly URL) so the same long-lived JS realm is reused, same
  // as a real user clicking around.
  await page.goto('/metrics/anomaly');
  const chart = page.locator('#metricsAnomalyChart');
  const clearFiltersLink = page.locator('form[method="get"] a[href="/metrics/anomaly"]');

  await expect(chart.locator('canvas')).toHaveCount(1);

  for (let i = 0; i < 3; i++) {
    await clearFiltersLink.click();
    await page.waitForLoadState('networkidle');
    await expect(chart.locator('canvas')).toHaveCount(1);
  }
});
