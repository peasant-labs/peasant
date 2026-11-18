import tseslint from 'typescript-eslint'
import unstring from 'eslint-plugin-unstring'

export default [
  {
    ignores: [
      '**/*.test.{ts,tsx}', '**/*.spec.{ts,tsx}', '**/*.stories.{ts,tsx}',
      '**/*.type-test.{ts,tsx}', '**/*.fixture.{ts,tsx}', '**/*fixtures*.{ts,tsx}',
      '**/*.generated.{ts,tsx}', '**/__tests__/**', '**/testdata/**', 'src/test/**',
      'src/lib/session-detail/mock-redactions.ts', 'src/lib/share/mock-data.ts',
    ],
    files: ['src/**/*.{ts,tsx}'],
    linterOptions: { noInlineConfig: true },
    languageOptions: {
      parser: tseslint.parser,
      parserOptions: { project: './tsconfig.eslint.json', tsconfigRootDir: import.meta.dirname },
    },
    plugins: { unstring },
    rules: { 'unstring/closed-set-literal': 'error' },
  },
]
