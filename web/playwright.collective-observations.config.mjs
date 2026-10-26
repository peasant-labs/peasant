import { defineConfig } from '@playwright/test';
import { accessSync, constants, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { parse } from 'yaml';
const fixtures = parse(readFileSync(new URL('./scripts/journey/testdata/collective-observations.yaml', import.meta.url), 'utf8'));
if (fixtures.projectNames.some(name => !fixtures.projects.some(project => project.name === name))) throw new Error('Required named theme/viewport project missing');
const chrome = process.env.CHROME_PATH;
if (!chrome) throw new Error('CHROME_PATH is required; install Chromium explicitly through the documented Linux Nix shell');
accessSync(chrome, constants.X_OK);
if (!process.env.OBSERVATIONS_APP_URL || !process.env.OBSERVATIONS_ARTIFACTS) throw new Error('Use journey:collective-observations: the owned runner must build, seed, boot and verify the actual embedded binary before Playwright');
export default defineConfig({
  testDir: './scripts/journey', testMatch: 'collective-observations.journey.mjs',
  workers: 1, retries: 0, timeout: 60000, globalTimeout: 300000, fullyParallel: false,
  expect: { timeout: 10000 },
  outputDir: join(process.env.OBSERVATIONS_ARTIFACTS, 'test-results'),
  reporter: [['list'], ['json', { outputFile: join(process.env.OBSERVATIONS_ARTIFACTS, 'playwright.json') }]],
  use: { baseURL: process.env.OBSERVATIONS_APP_URL, browserName: 'chromium', launchOptions: { executablePath: chrome }, trace: 'retain-on-failure', screenshot: 'only-on-failure', reducedMotion: 'reduce' },
  projects: fixtures.projects.map(project => ({ name: project.name, metadata: { theme: project.theme, surface: project.surface }, use: { viewport: { width: project.width, height: project.height }, isMobile: project.mobile, hasTouch: project.mobile } })),
});
