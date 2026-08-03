import { defineConfig, devices } from '@playwright/test';

const PORT = process.env.SOBS_E2E_PORT ? Number(process.env.SOBS_E2E_PORT) : 48173;
const BASE_URL = `http://127.0.0.1:${PORT}`;

// Boots a real sobs server against the frozen "base" fixture corpus (see
// scripts/e2e_server.py and go/goldenreplay/replay_test.go, which this mirrors) so specs
// exercise real seeded data over real HTTP rather than an empty database or mocked routes.
export default defineConfig({
  testDir: './e2e',
  // Serial, not parallel: every spec shares ONE server/database instance (see webServer
  // below), and a couple of specs perform real mutations (confirm-dialog.spec.ts deletes
  // the one seeded dashboard) that would race against anything else reading the same page
  // concurrently. This is a correctness suite, not a load test — determinism over speed.
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['github'], ['html', { open: 'never' }]] : 'list',
  use: {
    baseURL: BASE_URL,
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  webServer: {
    command: `python3 scripts/e2e_server.py --port ${PORT}`,
    url: `${BASE_URL}/healthz`,
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
    stdout: 'pipe',
    stderr: 'pipe',
  },
});
