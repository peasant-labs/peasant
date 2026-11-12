import { defineConfig } from '@playwright/test';
import { accessSync, constants, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { parse } from 'yaml';

const fixture = parse(readFileSync(new URL('./scripts/journey/testdata/kickstart.yaml', import.meta.url), 'utf8'));
const names = fixture.projects.map((project) => project.name);
if (new Set(names).size !== names.length) throw new Error('The setup journey names a theme project twice');
const required = ['KICKSTART_APP_URL', 'KICKSTART_ARTIFACTS', 'KICKSTART_CONFIG_PATH', 'KICKSTART_HOME', 'KICKSTART_SEEDED'];
const missing = required.filter((name) => !process.env[name]);
if (missing.length) throw new Error(`Use journey:kickstart: the owned runner builds the binary, seeds the computer and starts the server before Playwright. Missing ${missing.join(', ')}`);
const chrome = process.env.CHROME_PATH;
if (!chrome) throw new Error('CHROME_PATH must name an explicitly installed Chromium; use the repository Nix shell, no browser is downloaded');
accessSync(chrome, constants.X_OK);

export default defineConfig({
  testDir: './scripts/journey',
  testMatch: 'kickstart.journey.mjs',
  workers: 1,
  retries: 0,
  fullyParallel: false,
  timeout: 120000,
  globalTimeout: 600000,
  expect: { timeout: 10000 },
  outputDir: join(process.env.KICKSTART_ARTIFACTS, 'test-results'),
  reporter: [['list'], ['json', { outputFile: join(process.env.KICKSTART_ARTIFACTS, 'playwright.json') }]],
  use: {
    baseURL: process.env.KICKSTART_APP_URL,
    browserName: 'chromium',
    launchOptions: { executablePath: chrome },
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    reducedMotion: 'reduce',
  },
  projects: fixture.projects.map((project) => ({
    name: project.name,
    metadata: { theme: project.theme },
    use: { viewport: { width: project.width, height: project.height } },
  })),
});
