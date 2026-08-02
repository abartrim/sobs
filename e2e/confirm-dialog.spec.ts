import { test, expect, Page, Locator } from '@playwright/test';

// Regression coverage for a real bug found while building the htmx shell: htmx makes its own
// request-issuing decision on 'submit' and does NOT respect a competing listener's
// preventDefault() the way a plain (non-boosted) form did — so the old data-confirm-message
// guard let a delete fire before the user ever saw the confirm modal. The fix hooks the
// cancelable htmx:confirm event instead (see templates/base.html). These two tests are the
// regression check: cancel must fire zero requests, confirm must fire exactly one.
//
// Each test creates its own throwaway dashboard via the New Dashboard modal (the "New
// Dashboard" form is itself hx-boost="false", a full page nav/POST) rather than depending on
// a pre-seeded fixture dashboard — go/testdata/fixtures/base.tar.gz's seeded
// "Example Derived Signals" dashboard turned out to be unreliable: its sobs_dashboards
// MergeTree part gets detached as broken-on-start by this machine's chdb on every open (a
// pre-existing fixture/environment issue, unrelated to htmx — filed separately). Creating our
// own dashboard sidesteps that entirely and is arguably the more robust test design anyway.
test.describe.configure({ mode: 'serial' });

async function createDashboard(page: Page, name: string): Promise<Locator> {
  await page.goto('/dashboards');
  await page.locator('[data-bs-target="#newDashboardModal"]').first().click();
  await page.locator('#newDashboardModal input[name="name"]').fill(name);
  await page.locator('#newDashboardModal button[type="submit"]').click();
  await page.waitForURL(/\/dashboards\/[0-9a-f-]+$/);

  await page.goto('/dashboards');
  const card = page.locator('.card', { hasText: name });
  await expect(card).toBeVisible();
  return card;
}

test('canceling a confirm dialog fires zero requests', async ({ page }) => {
  const card = await createDashboard(page, 'E2E Cancel Target');

  const deleteRequests: string[] = [];
  page.on('request', (req) => {
    if (req.method() === 'POST' && req.url().includes('/dashboards/')) {
      deleteRequests.push(req.url());
    }
  });

  await card.locator('button[type="submit"]').click(); // the trash-icon delete button
  await expect(page.locator('#sobsConfirmModal')).toBeVisible();

  await page.locator('#sobsConfirmModal .modal-footer button', { hasText: 'Cancel' }).click();
  await expect(page.locator('#sobsConfirmModal')).toBeHidden();

  expect(deleteRequests).toHaveLength(0);
  await expect(card).toBeVisible(); // still there
});

test('confirming a confirm dialog fires exactly one request and removes the card', async ({ page }) => {
  const card = await createDashboard(page, 'E2E Confirm Target');

  const deleteRequests: string[] = [];
  page.on('request', (req) => {
    if (req.method() === 'POST' && req.url().includes('/dashboards/')) {
      deleteRequests.push(req.url());
    }
  });

  await card.locator('button[type="submit"]').click();
  await expect(page.locator('#sobsConfirmModal')).toBeVisible();

  await page.locator('#sobsConfirmModalOkBtn').click();
  await page.waitForLoadState('networkidle');

  expect(deleteRequests).toHaveLength(1);
  await expect(page.locator('.card', { hasText: 'E2E Confirm Target' })).toHaveCount(0);
});
