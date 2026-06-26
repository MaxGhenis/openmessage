const { defineConfig } = require('@playwright/test');

const port = process.env.OPENMESSAGES_E2E_PORT || '7010';
const baseURL = `http://127.0.0.1:${port}`;

module.exports = defineConfig({
  testDir: './e2e',
  fullyParallel: false,
  retries: process.env.CI ? 2 : 0,
  timeout: 30_000,
  use: {
    baseURL,
    headless: true,
    serviceWorkers: 'block',
    trace: 'retain-on-failure',
  },
  webServer: {
    command: 'go run ./cmd/e2e-server',
    cwd: __dirname,
    env: {
      ...process.env,
      OPENMESSAGES_E2E_PORT: port,
    },
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
    url: `${baseURL}/healthz`,
  },
  workers: 1,
});
