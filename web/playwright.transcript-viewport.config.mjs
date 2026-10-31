import { defineConfig } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { parse } from 'yaml';
const fixture = parse(readFileSync(new URL('./scripts/journey/testdata/transcript-viewport.yaml', import.meta.url), 'utf8'));
const names = fixture.projects.map(p => p.name);
if (new Set(names).size !== names.length || fixture.requiredNames.some(name => !names.includes(name))) throw new Error('Missing or duplicated required viewport scenario');
if (!process.env.CHROME_PATH || !process.env.PEASANT_ORIGIN || !process.env.PEASANT_BIN) throw new Error('Set CHROME_PATH, PEASANT_ORIGIN and PEASANT_BIN to the built mock-backed app');
export default defineConfig({
  testDir: './scripts/journey', testMatch: 'transcript-viewport.journey.mjs', workers: 1, retries: 0,
  timeout: 60000, reporter: 'list',
  use: { baseURL: process.env.PEASANT_ORIGIN, launchOptions: { executablePath: process.env.CHROME_PATH }, reducedMotion: 'reduce' },
  projects: fixture.projects.map(p => ({ name: p.name, metadata: { theme: p.theme }, use: { viewport: { width: p.width, height: p.height } } })),
});
