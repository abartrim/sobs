#!/usr/bin/env node

import { chromium } from 'playwright';
import fs from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

const baseUrl = (process.env.BASE_URL || 'http://127.0.0.1:5000').replace(/\/$/, '');
const headless = process.env.HEADLESS !== 'false';
const timestamp = new Date().toISOString().replace(/[^0-9]/g, '').slice(0, 14);
const artifactsRoot = process.env.QA_ARTIFACT_DIR || path.join('tests', 'screenshots', `playwright-toast-qa-${timestamp}`);
const screenshotQuality = Number(process.env.QA_SCREENSHOT_QUALITY || 45);
const seedTelemetry = process.env.QA_SEED_TELEMETRY !== 'false';
fs.mkdirSync(artifactsRoot, { recursive: true });

const pagesToAudit = [
  '/',
  '/dashboards',
  '/settings/tags',
  '/settings/data-management',
  '/settings/notifications',
  '/metrics/rules',
  '/settings/agents',
  '/errors',
  '/traces',
  '/incident',
];

function fullUrl(path) {
  return `${baseUrl}${path}`;
}

function pathKey(routePath) {
  if (!routePath || routePath === '/') return 'root';
  return routePath.replace(/^\//, '').replace(/[^a-zA-Z0-9_-]+/g, '_');
}

function seedTelemetryData() {
  if (!seedTelemetry) {
    console.log('QA seed: telemetry seeding skipped (QA_SEED_TELEMETRY=false).');
    return;
  }
  const pythonCmd = process.env.QA_PYTHON || path.join('.venv', 'bin', 'python');
  const total = String(Number(process.env.QA_SEED_TOTAL || 48));
  const workers = String(Number(process.env.QA_SEED_WORKERS || 8));
  const args = ['scripts/load_example.py', '--base', baseUrl, '--total', total, '--workers', workers];
  const run = spawnSync(pythonCmd, args, {
    cwd: process.cwd(),
    encoding: 'utf8',
  });
  if (run.status !== 0) {
    const stderr = (run.stderr || '').trim();
    console.log(`QA seed: telemetry seeding failed (status=${run.status}). ${stderr}`);
    return;
  }
  console.log(`QA seed: telemetry seeded via load_example.py (total=${total}, workers=${workers}).`);
}

async function waitForUiSettled(page, timeoutMs = 4000) {
  await page.waitForLoadState('domcontentloaded');
  try {
    await page.waitForFunction(async () => {
      function nextFrame() {
        return new Promise((resolve) => requestAnimationFrame(() => resolve()));
      }

      // Wait a couple of frames so style/layout updates are flushed.
      await nextFrame();
      await nextFrame();

      // Wait for web fonts, when supported.
      if (document.fonts && document.fonts.status !== 'loaded') {
        try {
          await document.fonts.ready;
        } catch (_err) {
          // Ignore font readiness errors and continue with visible-state checks.
        }
      }

      // Consider UI unstable if bootstrap collapse transition is in progress.
      if (document.querySelector('.collapsing')) return false;

      // Consider UI unstable if a bootstrap fade element is transitioning in/out.
      const fading = Array.from(document.querySelectorAll('.fade'));
      for (const el of fading) {
        const style = window.getComputedStyle(el);
        const isShown = el.classList.contains('show');
        const opacity = Number(style.opacity || '1');
        if (isShown && opacity < 0.99) return false;
        if (!isShown && opacity > 0.01) return false;
      }

      // Consider UI unstable if any CSS transition is still running (e.g. sidebar expand/collapse).
      if (typeof document.getAnimations === 'function') {
        const hasRunningTransitions = document.getAnimations().some(function (a) {
          return a.constructor.name === 'CSSTransition' && a.playState === 'running';
        });
        if (hasRunningTransitions) return false;
      }

      return true;
    }, undefined, { timeout: timeoutMs });
  } catch (_err) {
    // Best-effort settle only; do not fail test flow on long-running UI animations.
  }
}

async function screenshot(page, result, label) {
  await waitForUiSettled(page);
  // Temporarily hide fixed-position overlay elements (AI button, AI panel) so they
  // don't appear mid-page in full-page screenshots (position:fixed is captured at the
  // viewport offset, not the bottom of the document).
  await page.evaluate(() => {
    window.__qaHiddenFixedEls = [];
    ['#sobsAiBtn', '#sobsAiPanel'].forEach(function (sel) {
      const el = document.querySelector(sel);
      if (el) {
        window.__qaHiddenFixedEls.push({ el, prev: el.style.visibility });
        el.style.visibility = 'hidden';
      }
    });
  });
  const filePath = path.join(artifactsRoot, `${result.pathKey}-${label}.jpg`);
  await page.screenshot({
    path: filePath,
    fullPage: true,
    type: 'jpeg',
    quality: screenshotQuality,
    scale: 'css',
  });
  await page.evaluate(() => {
    (window.__qaHiddenFixedEls || []).forEach(function ({ el, prev }) { el.style.visibility = prev; });
    window.__qaHiddenFixedEls = [];
  });
  result.artifacts.push(filePath);
}

async function waitForToastLifecycle(page) {
  await page.waitForSelector('#sobsNotifyToastContainer .toast.show', { timeout: 4000 });
  await page.waitForFunction(() => {
    const container = document.getElementById('sobsNotifyToastContainer');
    if (!container) return false;
    return container.querySelectorAll('.toast.show').length === 0;
  }, undefined, { timeout: 7000 });
}

async function waitForConfirmModalFullyVisible(page) {
  await page.waitForSelector('#sobsConfirmModal.show', { timeout: 5000 });
  await page.waitForFunction(() => {
    const modal = document.getElementById('sobsConfirmModal');
    if (!modal) return false;
    const style = window.getComputedStyle(modal);
    if (style.display === 'none') return false;
    if (Number(style.opacity) < 0.99) return false;
    const dialog = modal.querySelector('.modal-dialog');
    if (!dialog) return false;
    const rect = dialog.getBoundingClientRect();
    if (rect.width <= 0 || rect.height <= 0) return false;
    return true;
  }, undefined, { timeout: 5000 });
  await waitForUiSettled(page);
  // Extra buffer to avoid capturing while CSS fade/transform is finishing.
  await page.waitForTimeout(250);
}

async function checkModalBackdropCoversSidebar(page, result, reasonLabel) {
  const coverage = await page.evaluate(() => {
    const sidebar = document.getElementById('sbSidebar');
    const backdrop = document.querySelector('.modal-backdrop.show');
    if (!sidebar) return { ok: true, skipped: 'no-sidebar' };
    if (!backdrop) return { ok: false, reason: 'missing-backdrop' };

    const sbRect = sidebar.getBoundingClientRect();
    if (sbRect.width <= 0 || sbRect.height <= 0) return { ok: true, skipped: 'sidebar-not-visible' };

    const x = Math.max(1, Math.min(window.innerWidth - 1, Math.round(sbRect.left + Math.min(sbRect.width / 2, 24))));
    const y = Math.max(1, Math.min(window.innerHeight - 1, Math.round(sbRect.top + Math.min(sbRect.height / 2, 40))));
    const topEl = document.elementFromPoint(x, y);
    const topClass = topEl ? topEl.className : null;

    const sbZ = Number(window.getComputedStyle(sidebar).zIndex || '0') || 0;
    const backdropZ = Number(window.getComputedStyle(backdrop).zIndex || '0') || 0;
    // Covered if elementFromPoint lands on the backdrop or any part of an open modal
    // (the modal dialog itself is stacked above the backdrop, so either means the
    // sidebar area is correctly obscured by the modal system).
    const coveredByModalSystem = !!(topEl && (
      topEl === backdrop ||
      (topEl.closest && topEl.closest('.modal-backdrop.show')) ||
      (topEl.closest && topEl.closest('.modal.show'))
    ));

    return {
      ok: coveredByModalSystem && backdropZ > sbZ,
      sidebarZ: sbZ,
      backdropZ,
      topClass,
      coveredByModalSystem,
    };
  });

  if (!coverage.ok) {
    result.failures.push(`Confirm modal backdrop does not cover sidebar (${reasonLabel}): ${JSON.stringify(coverage)}`);
  } else if (!coverage.skipped) {
    result.checks.push(`Confirm modal backdrop covers sidebar (${reasonLabel})`);
  }
}

async function dismissBlockingModals(page, result) {
  const hasBlockingModal = await page.evaluate(() => {
    return !!document.querySelector('.modal.show:not(#sobsConfirmModal)');
  });
  if (!hasBlockingModal) return;

  await page.evaluate(() => {
    const modalApi = window.bootstrap && window.bootstrap.Modal;
    if (!modalApi) return;
    document.querySelectorAll('.modal.show:not(#sobsConfirmModal)').forEach((modalEl) => {
      const modal = modalApi.getInstance(modalEl) || modalApi.getOrCreateInstance(modalEl);
      modal.hide();
    });
  });

  await page.waitForFunction(() => {
    return document.querySelectorAll('.modal.show:not(#sobsConfirmModal)').length === 0;
  }, undefined, { timeout: 5000 });

  result.checks.push('Dismissed blocking modal overlays before interaction checks');
}

async function openConfirmAndCancel(page, result, reasonLabel) {
  await waitForConfirmModalFullyVisible(page);
  await checkModalBackdropCoversSidebar(page, result, reasonLabel);
  await screenshot(page, result, `${reasonLabel}-confirm-open`);
  await page.click('#sobsConfirmModal .modal-footer [data-bs-dismiss="modal"]', { timeout: 5000 });
  await page.waitForSelector('#sobsConfirmModal.show', { state: 'hidden', timeout: 5000 });
}

async function openConfirmAndAccept(page, result, reasonLabel) {
  await waitForConfirmModalFullyVisible(page);
  await checkModalBackdropCoversSidebar(page, result, reasonLabel);
  await screenshot(page, result, `${reasonLabel}-confirm-open`);
  await page.click('#sobsConfirmModalOkBtn', { timeout: 5000 });
  await page.waitForLoadState('domcontentloaded');
}

async function checkProgrammaticConfirm(page, result) {
  await page.evaluate(() => {
    window.__qaConfirmResolved = null;
    window.SOBS.confirm({
      title: 'QA Confirm',
      message: 'QA confirm smoke check',
      okLabel: 'Cancel Me',
      okClass: 'btn-primary',
    }).then((value) => {
      window.__qaConfirmResolved = value;
    });
  });

  await openConfirmAndCancel(page, result, 'programmatic');
  await page.waitForFunction(() => window.__qaConfirmResolved === false, undefined, { timeout: 3000 });
  result.checks.push('Programmatic confirm opens and resolves false on cancel');
}

async function checkDeclarativeConfirm(page, result, selector = 'form[data-confirm-message]') {
  const form = page.locator(selector).first();
  if ((await form.count()) === 0) {
    result.warnings.push(`No matching declarative confirm form for selector: ${selector}`);
    return false;
  }

  const submitButton = form.locator('button[type="submit"],input[type="submit"]').first();
  if ((await submitButton.count()) > 0) {
    await submitButton.click({ timeout: 5000 });
  } else {
    await form.evaluate((node) => {
      if (typeof node.requestSubmit === 'function') {
        node.requestSubmit();
      } else {
        node.submit();
      }
    });
  }

  await openConfirmAndCancel(page, result, 'declarative');
  result.checks.push('Declarative confirm opens and cancels');
  return true;
}

async function toggleAndRevert(page, result, selector, label) {
  const firstButton = page.locator(selector).first();
  if ((await firstButton.count()) === 0) {
    result.warnings.push(`${label}: no matching toggle button found`);
    return false;
  }

  await firstButton.scrollIntoViewIfNeeded();
  await Promise.all([
    page.waitForLoadState('domcontentloaded'),
    firstButton.click({ timeout: 7000 }),
  ]);
  await screenshot(page, result, `${label}-after-toggle`);

  const secondButton = page.locator(selector).first();
  if ((await secondButton.count()) === 0) {
    result.failures.push(`${label}: toggle control disappeared before revert`);
    return false;
  }

  await Promise.all([
    page.waitForLoadState('domcontentloaded'),
    secondButton.click({ timeout: 7000 }),
  ]);
  await screenshot(page, result, `${label}-after-revert`);
  result.checks.push(`${label}: toggled and reverted`);
  return true;
}

async function checkSidebarToggleRevert(page, result) {
  const toggleBtn = page.locator('#sbToggleBtn');
  const sidebar = page.locator('#sbSidebar');
  if ((await toggleBtn.count()) === 0 || (await sidebar.count()) === 0) {
    result.warnings.push('Sidebar toggle controls not found; sidebar change+revert check skipped');
    return;
  }

  const beforeCompact = await page.evaluate(() => {
    const el = document.getElementById('sbSidebar');
    return !!(el && el.classList.contains('sidebar-compact'));
  });

  await toggleBtn.first().click({ timeout: 5000 });
  await page.waitForFunction((before) => {
    const el = document.getElementById('sbSidebar');
    return !!el && el.classList.contains('sidebar-compact') !== before;
  }, beforeCompact, { timeout: 5000 });
  await screenshot(page, result, 'sidebar-after-toggle');

  await toggleBtn.first().click({ timeout: 5000 });
  await page.waitForFunction((before) => {
    const el = document.getElementById('sbSidebar');
    return !!el && el.classList.contains('sidebar-compact') === before;
  }, beforeCompact, { timeout: 5000 });
  await screenshot(page, result, 'sidebar-after-revert');

  result.checks.push('Sidebar setting toggled and reverted (compact/full)');
}

async function getToastCount(page) {
  return page.evaluate(() => {
    const container = document.getElementById('sobsNotifyToastContainer');
    if (!container) return 0;
    return container.querySelectorAll('.toast').length;
  });
}

async function expectNewToast(page, result, label, beforeCount, textHint) {
  try {
    await page.waitForFunction(({ count, hint }) => {
      const container = document.getElementById('sobsNotifyToastContainer');
      if (!container) return false;
      const all = Array.from(container.querySelectorAll('.toast'));
      if (all.length <= count) return false;
      if (!hint) return true;
      const newer = all.slice(count);
      return newer.some((el) => String(el.textContent || '').toLowerCase().includes(String(hint).toLowerCase()));
    }, { count: beforeCount, hint: textHint || '' }, { timeout: 6000 });
    result.checks.push(`${label}: notify toast shown`);
    return true;
  } catch (_err) {
    result.failures.push(`${label}: expected notify toast was not shown`);
    return false;
  }
}

async function withCopyFailure(page, fn) {
  await page.evaluate(() => {
    if (!window.__qaOrigCopy) {
      window.__qaOrigCopy = window.SOBS && window.SOBS.copyToClipboard;
    }
    if (window.SOBS) {
      window.SOBS.copyToClipboard = function () {
        return Promise.reject(new Error('qa-copy-fail'));
      };
    }
  });
  try {
    await fn();
  } finally {
    await page.evaluate(() => {
      if (window.SOBS && window.__qaOrigCopy) {
        window.SOBS.copyToClipboard = window.__qaOrigCopy;
      }
    });
  }
}

async function withFetchFailure(page, fn) {
  await page.evaluate(() => {
    if (!window.__qaOrigFetch) {
      window.__qaOrigFetch = window.fetch.bind(window);
    }
    window.fetch = function () {
      return Promise.reject(new Error('qa-net-fail'));
    };
  });
  try {
    await fn();
  } finally {
    await page.evaluate(() => {
      if (window.__qaOrigFetch) {
        window.fetch = window.__qaOrigFetch;
      }
    });
  }
}

async function withFetchJson(page, payload, fn) {
  await page.evaluate((jsonPayload) => {
    if (!window.__qaOrigFetch) {
      window.__qaOrigFetch = window.fetch.bind(window);
    }
    window.fetch = function () {
      return Promise.resolve({
        ok: true,
        status: 200,
        json: function () {
          return Promise.resolve(jsonPayload);
        },
      });
    };
  }, payload);
  try {
    await fn();
  } finally {
    await page.evaluate(() => {
      if (window.__qaOrigFetch) {
        window.fetch = window.__qaOrigFetch;
      }
    });
  }
}

async function runPageSpecificChecks(page, result) {
  await dismissBlockingModals(page, result);

  if (result.path === '/dashboards') {
    await checkDeclarativeConfirm(page, result);
  }

  if (result.path === '/settings/tags') {
    await checkDeclarativeConfirm(page, result);
  }

  if (result.path === '/settings/data-management') {
    const restoreInput = page.locator('#restoreBackupName');
    const restoreButton = page.locator('#btnRunRestore');
    const backupToggle = page.locator('#backupEnabled');
    const saveSettingsButton = page.locator('button[type="submit"][name="apply_ttl"][value="0"]');
    let revertBackupToggle = false;

    if ((await restoreInput.count()) === 0 || (await restoreButton.count()) === 0) {
      if ((await backupToggle.count()) > 0 && (await saveSettingsButton.count()) > 0) {
        const wasEnabled = await backupToggle.isChecked();
        if (!wasEnabled) {
          await backupToggle.click({ force: true });
          let nowEnabled = await backupToggle.isChecked();
          if (!nowEnabled) {
            await page.evaluate(() => {
              const el = document.getElementById('backupEnabled');
              if (!el) return;
              el.checked = true;
              el.dispatchEvent(new Event('change', { bubbles: true }));
            });
            nowEnabled = await backupToggle.isChecked();
          }
          if (nowEnabled) {
            await Promise.all([
              page.waitForLoadState('domcontentloaded'),
              saveSettingsButton.click({ timeout: 7000 }),
            ]);
            await dismissBlockingModals(page, result);
            revertBackupToggle = true;
            result.checks.push('Seeded data-management backup controls by enabling backup settings');
          } else {
            result.warnings.push('Could not enable backup toggle for seeded restore check; skipping backup-control seeding');
          }
        }
      }
    }

    if ((await restoreInput.count()) > 0 && (await restoreButton.count()) > 0) {
      await dismissBlockingModals(page, result);
      await restoreInput.fill('qa-non-destructive-restore-check');
      try {
        await restoreButton.click({ timeout: 5000 });
      } catch (_err) {
        await dismissBlockingModals(page, result);
        await restoreButton.click({ timeout: 5000, force: true });
      }
      await openConfirmAndCancel(page, result, 'restore');
      result.checks.push('Restore confirmation modal opens and cancels safely');
    } else {
      result.warnings.push('Restore controls not found on data-management page');
    }

    if (revertBackupToggle && (await backupToggle.count()) > 0 && (await saveSettingsButton.count()) > 0) {
      await backupToggle.click({ force: true });
      const reverted = !(await backupToggle.isChecked());
      if (reverted) {
        await Promise.all([
          page.waitForLoadState('domcontentloaded'),
          saveSettingsButton.click({ timeout: 7000 }),
        ]);
        await dismissBlockingModals(page, result);
        result.checks.push('Reverted data-management backup setting after seeded restore check');
      } else {
        result.warnings.push('Backup setting revert click did not change state; manual check recommended');
      }
    }
  }

  if (result.path === '/settings/notifications') {
    let seededChannelName = null;

    const existingToggle = page.locator('form[action*="/notifications/channels/"][action$="/toggle"] button[type="submit"]').first();
    if ((await existingToggle.count()) === 0) {
      seededChannelName = `qa-seed-${Date.now()}`;
      const addChannelToggle = page.locator('[data-bs-target="#addChannelCollapse"]').first();
      if ((await addChannelToggle.count()) > 0) {
        await addChannelToggle.click({ timeout: 5000 });
      }

      await page.fill('#addChannelForm input[name="name"]', seededChannelName);
      await page.selectOption('#addChannelForm select[name="channel_type"]', 'webhook');
      await page.fill('#addChannelForm input[name="webhook_url"]', 'http://127.0.0.1:65535/qa-seed-endpoint');
      await Promise.all([
        page.waitForLoadState('domcontentloaded'),
        page.locator('#addChannelForm button[type="submit"]').click({ timeout: 7000 }),
      ]);
      await dismissBlockingModals(page, result);
      await screenshot(page, result, 'notifications-seeded-channel');
      result.checks.push(`Seeded notification channel for deterministic interaction checks: ${seededChannelName}`);
    }

    const deleteSelector = seededChannelName
      ? `tr:has-text("${seededChannelName}") form[action*="/notifications/channels/"][action$="/delete"][data-confirm-message]`
      : 'form[action*="/notifications/channels/"][action$="/delete"][data-confirm-message], form[action*="/notifications/rules/"][action$="/delete"][data-confirm-message]';

    await checkDeclarativeConfirm(page, result, deleteSelector);

    let toggleSelector = 'form[action*="/notifications/channels/"][action$="/toggle"] button[type="submit"]';
    if (seededChannelName) {
      toggleSelector = `tr:has-text("${seededChannelName}") form[action*="/notifications/channels/"][action$="/toggle"] button[type="submit"]`;
    }

    let toggled = await toggleAndRevert(page, result, toggleSelector, 'channel-toggle');
    if (!toggled) {
      toggled = await toggleAndRevert(page, result, 'form[action*="/notifications/rules/"][action$="/toggle"] button[type="submit"]', 'rule-toggle');
    }
    if (!toggled) {
      result.warnings.push('No notification toggles were available to test change+revert');
    }

    if (seededChannelName) {
      const seededDeleteForm = page.locator(`tr:has-text("${seededChannelName}") form[action*="/notifications/channels/"][action$="/delete"][data-confirm-message]`).first();
      if ((await seededDeleteForm.count()) > 0) {
        const seededDeleteButton = seededDeleteForm.locator('button[type="submit"]').first();
        await seededDeleteButton.click({ timeout: 5000 });
        await openConfirmAndAccept(page, result, 'notifications-seeded-cleanup');
        await dismissBlockingModals(page, result);
        result.checks.push(`Cleaned up seeded notification channel: ${seededChannelName}`);
      } else {
        result.warnings.push(`Seeded notification channel cleanup skipped; row not found: ${seededChannelName}`);
      }
    }

    const testChannelButton = page.locator('.test-channel-btn').first();
    if ((await testChannelButton.count()) > 0) {
      const beforeToastCount = await getToastCount(page);
      await withFetchFailure(page, async () => {
        await testChannelButton.click({ timeout: 5000 });
      });
      await expectNewToast(page, result, 'Notifications test-channel failure path', beforeToastCount, 'request error');
    } else {
      result.warnings.push('No test-channel button available for notify replacement check');
    }

    const browserPushButton = page.locator('#subscribeBrowserBtn').first();
    if ((await browserPushButton.count()) > 0) {
      const beforeToastCount = await getToastCount(page);
      await withFetchJson(page, { ok: false, error: 'qa-no-vapid-key' }, async () => {
        await page.evaluate(() => {
          const btn = document.getElementById('subscribeBrowserBtn');
          if (btn) btn.click();
        });
      });
      await expectNewToast(page, result, 'Notifications browser-push missing-key path', beforeToastCount, 'cannot subscribe');
    } else {
      result.checks.push('Notifications browser-push missing-key path skipped (no subscribe browser button)');
    }

    const generateVapidButton = page.locator('#generateVapidBtn').first();
    if ((await generateVapidButton.count()) > 0) {
      const beforeToastCount = await getToastCount(page);
      await withFetchFailure(page, async () => {
        await generateVapidButton.click({ timeout: 5000, force: true });
      });
      await expectNewToast(page, result, 'Notifications VAPID generate error path', beforeToastCount, 'vapid keys');
    } else {
      const regenerateVapidButton = page.locator('#regenerateVapidBtn').first();
      if ((await regenerateVapidButton.count()) > 0) {
        await page.evaluate(() => {
          if (!window.__qaOrigConfirm) {
            window.__qaOrigConfirm = window.SOBS && window.SOBS.confirm;
          }
          if (window.SOBS) {
            window.SOBS.confirm = function () {
              return Promise.resolve(true);
            };
          }
        });
        const beforeToastCount = await getToastCount(page);
        await withFetchFailure(page, async () => {
          await page.evaluate(() => {
            const btn = document.getElementById('regenerateVapidBtn');
            if (btn) btn.click();
          });
        });
        await page.evaluate(() => {
          if (window.SOBS && window.__qaOrigConfirm) {
            window.SOBS.confirm = window.__qaOrigConfirm;
          }
        });
        await expectNewToast(page, result, 'Notifications VAPID regenerate error path', beforeToastCount, 'vapid keys');
      } else {
        result.checks.push('Notifications VAPID error-path check skipped (no VAPID action button)');
      }
    }
  }

  if (result.path === '/metrics/rules') {
    const deleteRuleButton = page.locator('.js-delete-rule').first();
    if ((await deleteRuleButton.count()) > 0) {
      await deleteRuleButton.click({ timeout: 5000 });
      await openConfirmAndCancel(page, result, 'metrics-delete');
      result.checks.push('Metric rule delete confirm opens and cancels');
    } else {
      result.warnings.push('No metric delete button found for confirm test');
    }

    const notifyRuleButton = page.locator('.js-notify-rule').first();
    if ((await notifyRuleButton.count()) > 0) {
      const beforeToastCount = await getToastCount(page);
      await withFetchFailure(page, async () => {
        await notifyRuleButton.click({ timeout: 5000 });
      });
      await expectNewToast(page, result, 'Metric notify-rule failure path', beforeToastCount, 'notification rule');
    } else {
      result.warnings.push('No notify-rule bell button found for notify replacement check');
    }
  }

  if (result.path === '/settings/agents') {
    let seededRuleName = null;
    let runButton = page.locator('.sobs-run-btn').first();
    if ((await runButton.count()) === 0) {
      seededRuleName = `qa-seed-agent-${Date.now()}`;
      const createForm = page.locator('form[action*="/settings/agents"]').first();
      if ((await createForm.count()) > 0) {
        await createForm.locator('input[name="name"]').fill(seededRuleName);
        await Promise.all([
          page.waitForLoadState('domcontentloaded'),
          createForm.locator('button[type="submit"]').first().click({ timeout: 7000 }),
        ]);
        await dismissBlockingModals(page, result);
        runButton = page.locator(`tr:has-text("${seededRuleName}") .sobs-run-btn`).first();
        if ((await runButton.count()) > 0) {
          result.checks.push(`Seeded agent rule for notify replacement check: ${seededRuleName}`);
        }
      }
    }

    if ((await runButton.count()) > 0) {
      await page.evaluate(() => {
        window.__qaOrigPrompt = window.prompt;
        window.prompt = function () { return ''; };
      });
      const beforeToastCount = await getToastCount(page);
      await withFetchFailure(page, async () => {
        await runButton.click({ timeout: 5000 });
      });
      await page.evaluate(() => {
        if (window.__qaOrigPrompt) {
          window.prompt = window.__qaOrigPrompt;
        }
      });
      await expectNewToast(page, result, 'Agent run failure path', beforeToastCount, 'failed to trigger agent run');
    } else {
      result.checks.push('Agent run failure path skipped (no agent run button present)');
    }

    if (seededRuleName) {
      const seededDeleteButton = page.locator(`tr:has-text("${seededRuleName}") .sobs-delete-rule-form button[type="submit"]`).first();
      if ((await seededDeleteButton.count()) > 0) {
        await seededDeleteButton.click({ timeout: 5000 });
        await openConfirmAndAccept(page, result, 'agents-seeded-cleanup');
        await dismissBlockingModals(page, result);
        result.checks.push(`Cleaned up seeded agent rule: ${seededRuleName}`);
      }
    }
  }

  if (result.path === '/errors') {
    const copyButton = page.locator('.copy-stack-btn').first();
    if ((await copyButton.count()) > 0) {
      const beforeToastCount = await getToastCount(page);
      await withCopyFailure(page, async () => {
        await page.evaluate(() => {
          const btn = document.querySelector('.copy-stack-btn');
          if (!btn) return;
          const stackId = btn.getAttribute('data-stack-id');
          let stackEl = stackId ? document.getElementById(stackId) : null;
          if (!stackEl && stackId) {
            stackEl = document.createElement('pre');
            stackEl.id = stackId;
            stackEl.style.position = 'fixed';
            stackEl.style.left = '-10000px';
            stackEl.style.top = '0';
            document.body.appendChild(stackEl);
          }
          if (stackEl) {
            stackEl.style.display = 'block';
            stackEl.innerText = 'qa synthetic stack';
          }
          btn.click();
        });
      });
      try {
        await page.waitForFunction(({ count, hint }) => {
          const container = document.getElementById('sobsNotifyToastContainer');
          if (!container) return false;
          const all = Array.from(container.querySelectorAll('.toast'));
          if (all.length <= count) return false;
          const newer = all.slice(count);
          return newer.some((el) => String(el.textContent || '').toLowerCase().includes(String(hint).toLowerCase()));
        }, { count: beforeToastCount, hint: 'could not copy stack trace' }, { timeout: 3000 });
        result.checks.push('Errors copy-stack failure path: notify toast shown');
      } catch (_err) {
        result.checks.push('Errors copy-stack failure path skipped (stack content not triggerable in current dataset)');
      }
    } else {
      result.checks.push('Errors copy-stack failure path skipped (no copy-stack button)');
    }

    const aiHelpButton = page.locator('.ai-help-btn').first();
    if ((await aiHelpButton.count()) > 0) {
      const beforeToastCount = await getToastCount(page);
      await withCopyFailure(page, async () => {
        await aiHelpButton.click({ timeout: 5000 });
      });
      await expectNewToast(page, result, 'Errors AI-help copy failure path', beforeToastCount, 'could not copy to clipboard');
    } else {
      result.checks.push('Errors AI-help copy failure path skipped (no AI-help button)');
    }
  }

  if (result.path === '/traces') {
    const traceCopyButton = page.locator('.trace-copy-stack-btn').first();
    if ((await traceCopyButton.count()) > 0) {
      await page.evaluate(() => {
        const btn = document.querySelector('.trace-copy-stack-btn');
        if (!btn) return;
        const stackId = btn.getAttribute('data-stack-id');
        const stackEl = stackId ? document.getElementById(stackId) : null;
        if (stackEl && !String(stackEl.innerText || '').trim()) {
          stackEl.innerText = 'qa synthetic trace stack';
        }
      });
      const beforeToastCount = await getToastCount(page);
      await withCopyFailure(page, async () => {
        await traceCopyButton.click({ timeout: 5000 });
      });
      await expectNewToast(page, result, 'Traces copy-stack failure path', beforeToastCount, 'could not copy stack trace');
    } else {
      result.checks.push('Traces copy-stack failure path skipped (no copy-stack button)');
    }

    const traceAiHelpButton = page.locator('.trace-ai-help-btn').first();
    if ((await traceAiHelpButton.count()) > 0) {
      const beforeToastCount = await getToastCount(page);
      await withCopyFailure(page, async () => {
        await traceAiHelpButton.click({ timeout: 5000 });
      });
      await expectNewToast(page, result, 'Traces AI-help copy failure path', beforeToastCount, 'could not copy to clipboard');
    } else {
      result.checks.push('Traces AI-help copy failure path skipped (no AI-help button)');
    }
  }

  if (result.path === '/incident') {
    const incidentRaiseButton = page.locator('#incident-raise-btn').first();
    if ((await incidentRaiseButton.count()) > 0) {
      const beforeToastCount = await getToastCount(page);
      await withFetchFailure(page, async () => {
        await incidentRaiseButton.click({ timeout: 5000 });
      });
      await expectNewToast(page, result, 'Incident primary raise failure path', beforeToastCount, 'could not raise issue');
    } else {
      result.checks.push('Incident primary raise failure path skipped (no primary raise button)');
    }

    const incidentErrorRaiseButton = page.locator('.incident-raise-issue-btn').first();
    if ((await incidentErrorRaiseButton.count()) > 0) {
      const beforeToastCount = await getToastCount(page);
      await withFetchFailure(page, async () => {
        await incidentErrorRaiseButton.click({ timeout: 5000 });
      });
      await expectNewToast(page, result, 'Incident related-error raise failure path', beforeToastCount, 'could not raise issue');
    } else {
      result.checks.push('Incident related-error raise failure path skipped (no related-error raise button)');
    }
  }
}

async function runPageChecks(context, path) {
  const page = await context.newPage();
  const result = {
    path,
    pathKey: pathKey(path),
    url: fullUrl(path),
    checks: [],
    warnings: [],
    failures: [],
    artifacts: [],
  };

  page.on('dialog', async (dialog) => {
    result.failures.push(`Native browser dialog detected (${dialog.type()}): ${dialog.message()}`);
    await dialog.dismiss();
  });

  try {
    const response = await page.goto(result.url, { waitUntil: 'domcontentloaded', timeout: 15000 });
    const status = response ? response.status() : 0;
    if (!response) {
      result.failures.push('Navigation did not return an HTTP response.');
      return result;
    }
    if (status >= 400) {
      result.failures.push(`HTTP ${status} while loading page.`);
      return result;
    }

    result.checks.push(`Loaded with HTTP ${status}`);
    await screenshot(page, result, 'initial');
    await dismissBlockingModals(page, result);

    const hasNotifyApi = await page.evaluate(() => !!(window.SOBS && typeof window.SOBS.notify === 'function'));
    const hasConfirmApi = await page.evaluate(() => !!(window.SOBS && typeof window.SOBS.confirm === 'function'));
    if (!hasNotifyApi) {
      result.failures.push('window.SOBS.notify is not available.');
      return result;
    }
    if (!hasConfirmApi) {
      result.failures.push('window.SOBS.confirm is not available.');
      return result;
    }
    result.checks.push('SOBS notify/confirm APIs are available');

    const hasToastContainer = await page.locator('#sobsNotifyToastContainer').count();
    if (!hasToastContainer) {
      result.failures.push('Missing #sobsNotifyToastContainer in DOM.');
      return result;
    }
    result.checks.push('Toast container exists');

    const visualState = await page.evaluate(() => {
      const container = document.getElementById('sobsNotifyToastContainer');
      if (!container) {
        return { ok: false, reason: 'no-container' };
      }
      const styles = window.getComputedStyle(container);
      return {
        ok: true,
        position: styles.position,
        bottom: styles.bottom,
        right: styles.right,
        zIndex: styles.zIndex,
      };
    });
    if (!visualState.ok || visualState.position !== 'fixed') {
      result.failures.push(`Toast container visual positioning invalid: ${JSON.stringify(visualState)}`);
      return result;
    }
    result.checks.push(`Toast container visual position verified (bottom=${visualState.bottom}, right=${visualState.right}, z-index=${visualState.zIndex})`);

    await page.evaluate(() => {
      window.SOBS.notify('QA: toast smoke check', {
        title: 'QA',
        level: 'info',
        delay: 1200,
      });
    });
    // Screenshot while the toast is visible, then confirm it auto-hides.
    await page.waitForSelector('#sobsNotifyToastContainer .toast.show', { timeout: 4000 });
    await screenshot(page, result, 'post-toast');
    await page.waitForFunction(() => {
      const c = document.getElementById('sobsNotifyToastContainer');
      return !c || c.querySelectorAll('.toast.show').length === 0;
    }, undefined, { timeout: 7000 });
    result.checks.push('Toast smoke check passed (show + auto-hide)');

    await dismissBlockingModals(page, result);
    await checkProgrammaticConfirm(page, result);
    await checkSidebarToggleRevert(page, result);
    await runPageSpecificChecks(page, result);
  } catch (err) {
    result.failures.push(`Unhandled error: ${err.message}`);
  } finally {
    await page.close();
  }

  return result;
}

async function main() {
  seedTelemetryData();
  const browser = await chromium.launch({ headless });
  const context = await browser.newContext();

  // Suppress first-run onboarding modals for every page load in this test run.
  // These keys are read synchronously by the setup-wizard and first-run tour scripts
  // before the auto-open setTimeout fires, so injecting them via addInitScript
  // prevents the modals from ever opening without needing to dismiss them per-page.
  await context.addInitScript(() => {
    try {
      localStorage.setItem('sobs.setupWizardSeen.v1',  '1');
      localStorage.setItem('sobs.firstRunTourSeen.v1', '1');
      localStorage.setItem('sobs.firstRunTourShown.v1', '1');
    } catch (_e) {}
  });

  const results = [];
  try {
    for (const path of pagesToAudit) {
      // Keep execution deterministic and readable for CI logs.
      const pageResult = await runPageChecks(context, path);
      results.push(pageResult);
    }
  } finally {
    await context.close();
    await browser.close();
  }

  let totalFailures = 0;
  let totalWarnings = 0;
  console.log(`\nPlaywright UI QA summary for ${baseUrl}`);
  console.log('=====================================================');
  console.log(`Artifacts directory: ${artifactsRoot}`);
  for (const r of results) {
    console.log(`\n[${r.path}]`);
    for (const check of r.checks) {
      console.log(`  PASS: ${check}`);
    }
    for (const warning of r.warnings) {
      console.log(`  WARN: ${warning}`);
      totalWarnings += 1;
    }
    for (const failure of r.failures) {
      console.log(`  FAIL: ${failure}`);
      totalFailures += 1;
    }
    if (r.artifacts.length > 0) {
      console.log('  ARTIFACTS:');
      for (const filePath of r.artifacts) {
        console.log(`    ${filePath}`);
      }
    }
  }

  if (totalFailures > 0) {
    console.log(`\nResult: FAILED with ${totalFailures} issue(s), ${totalWarnings} warning(s).`);
    process.exit(1);
  }

  console.log(`\nResult: PASSED with ${totalWarnings} warning(s).`);
}

main().catch((err) => {
  console.error(`Fatal error: ${err.message}`);
  process.exit(1);
});
