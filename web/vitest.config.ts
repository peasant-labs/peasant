import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';
import path from 'path';

const transcriptMutation = process.env.PEASANT_TRANSCRIPT_MUTATION_JSON
  ? JSON.parse(process.env.PEASANT_TRANSCRIPT_MUTATION_JSON) as { name: string; find: string; replace: string }
  : null;
const sourceMutation = process.env.PEASANT_SOURCE_MUTATION_JSON
  ? JSON.parse(process.env.PEASANT_SOURCE_MUTATION_JSON) as { name: string; target: string; find: string; replace: string }
  : null;

const isolatedTranscriptMutationPlugin = {
  name: 'peasant-isolated-transcript-mutation',
  enforce: 'pre' as const,
  transform(code: string, id: string) {
    if (!transcriptMutation || !id.split('?')[0].endsWith('/src/components/session-detail/v2/SessionDetailV2.tsx')) return null;
    const count = code.split(transcriptMutation.find).length - 1;
    if (count !== 1) throw new Error(`${transcriptMutation.name}: isolated mutation anchor must occur exactly once, received ${count}`);
    return { code: code.replace(transcriptMutation.find, transcriptMutation.replace), map: null };
  },
};

const isolatedSourceMutationPlugin = {
  name: 'peasant-isolated-source-mutation',
  enforce: 'pre' as const,
  transform(code: string, id: string) {
    if (!sourceMutation || !id.split('?')[0].endsWith(sourceMutation.target)) return null;
    const count = code.split(sourceMutation.find).length - 1;
    if (count !== 1) throw new Error(`${sourceMutation.name}: isolated source mutation anchor must occur exactly once, received ${count}`);
    return { code: code.replace(sourceMutation.find, sourceMutation.replace), map: null };
  },
};

export default defineConfig({
  // The React plugin and Vitest resolve different Vite type versions, while `vite` is not a
  // direct dependency. Keep the compatibility cast local to this configuration boundary.
  plugins: [isolatedTranscriptMutationPlugin, isolatedSourceMutationPlugin, react() as any],
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.{test,spec}.{js,jsx,ts,tsx}'],
    // Inline the design-system package and lucide-react so Vitest transforms both against
    // this app's single React instance. The optimized Next build already deduplicates them;
    // this setting protects only the test transform pipeline.
    server: {
      deps: {
        inline: [/@peasant-labs\/fairtrade/, /lucide-react/],
      },
    },
  },
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
    // Force hooks inside dependency components to resolve this app's React and icon context.
    dedupe: ['react', 'react-dom', 'lucide-react'],
  },
});
