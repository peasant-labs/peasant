import { defineConfig } from 'vitest/config';
export default defineConfig({ test: { environment: 'node', include: ['scripts/journey/api-binary.test.mjs'], fileParallelism: false, testTimeout: 60000, hookTimeout: 600000 } });
