import { test, expect } from '@playwright/test';
import { markNoFullReload, expectNoFullReload, expectSingleMainContent } from './helpers';

// The "base" fixture corpus seeds config tables (dashboards, reports, ...) but no raw
// telemetry events at all — otel_traces has zero rows, so the sort-column headers (which
// live inside traces.html's `{% elif spans %}` branch, unrendered when there are no spans)
// wouldn't exist for a test to click. This test ingests two real spans of its own via the
// OTLP HTTP JSON endpoint (POST /v1/traces) before running, rather than depending on any
// pre-seeded row — same reasoning as confirm-dialog.spec.ts's self-contained dashboard.
// Pagination still isn't exercised here: two rows is enough for sort headers to render and
// order to be checkable, not enough to produce a second page.
const FAKE_EPOCH_NS = 1704164645000000000; // matches scripts/e2e_server.py's SOBS_FAKE_EPOCH

function otlpSpan(name: string, durationMs: number, traceId: string, spanId: string) {
  const startNs = FAKE_EPOCH_NS;
  const endNs = startNs + durationMs * 1_000_000;
  return {
    resource: { attributes: [{ key: 'service.name', value: { stringValue: 'checkout' } }] },
    scopeSpans: [
      {
        spans: [
          {
            traceId,
            spanId,
            name,
            startTimeUnixNano: String(startNs),
            endTimeUnixNano: String(endNs),
            status: { code: 1 },
          },
        ],
      },
    ],
  };
}

test.beforeAll(async ({ request }) => {
  const res = await request.post('/v1/traces', {
    data: {
      resourceSpans: [
        otlpSpan('fast-span', 10, '1'.repeat(32), '1'.repeat(16)),
        otlpSpan('slow-span', 9000, '2'.repeat(32), '2'.repeat(16)),
      ],
    },
  });
  expect(res.ok()).toBe(true);
});

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
