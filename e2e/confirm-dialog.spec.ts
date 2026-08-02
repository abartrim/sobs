import { test, expect } from '@playwright/test';

// Regression coverage for a real bug found while building the htmx shell: htmx makes its own
// request-issuing decision on 'submit' and does NOT respect a competing listener's
// preventDefault() the way a plain (non-boosted) form did — so the old data-confirm-message
// guard let a delete fire before the user ever saw the confirm modal. The fix hooks the
// cancelable htmx:confirm event instead (see templates/base.html). These two tests are the
// regression check: cancel must fire zero requests, confirm must fire exactly one.
//
// Serial within this file: both tests act on the single seeded dashboard
// ("Example Derived Signals" — see go/testdata/fixtures/base_dump.json), and the confirm
// test deletes it, so cancel must run first and no other spec should depend on that
// dashboard still existing afterward.
test.describe.configure({ mode: 'serial' });

test('canceling a confirm dialog fires zero requests', async ({ page }) => {
  await page.goto('/dashboards');
  const card = page.locator('.card', { hasText: 'Example Derived Signals' });
  await expect(card).toBeVisible();

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
  await page.goto('/dashboards');
  const card = page.locator('.card', { hasText: 'Example Derived Signals' });
  await expect(card).toBeVisible();

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
  await expect(page.locator('.card', { hasText: 'Example Derived Signals' })).toHaveCount(0);
});
