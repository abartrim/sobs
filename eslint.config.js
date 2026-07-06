'use strict';

// ESLint flat config for the integration snippets in examples/ (how a third-party app talks to
// SOBS from Node.js / the browser) — NOT the shipped RUM bundle source, which is type-checked
// separately via tsc (see tsconfig.rum.json). Kept to core ESLint rules only (no @eslint/js or
// plugin packages), matching the project's minimal-dependency approach (see go/DEPENDENCIES.md
// for the same philosophy on the Go side).
module.exports = [
  {
    files: ['examples/nodejs/**/*.js'],
    languageOptions: {
      ecmaVersion: 2022,
      sourceType: 'commonjs',
      globals: {
        require: 'readonly',
        module: 'readonly',
        process: 'readonly',
        console: 'readonly',
      },
    },
    rules: {
      // caughtErrorsIgnorePattern: examples use `catch (_err) {}` to deliberately discard an
      // error (best-effort cleanup, etc.) — the leading underscore is the signal, here and below.
      'no-unused-vars': ['error', { caughtErrorsIgnorePattern: '^_' }],
      'no-undef': 'error',
      'no-redeclare': 'error',
      'no-const-assign': 'error',
    },
  },
  {
    files: ['examples/rum/**/*.js'],
    languageOptions: {
      ecmaVersion: 2022,
      sourceType: 'script',
      globals: {
        window: 'readonly',
        document: 'readonly',
        fetch: 'readonly',
        console: 'readonly',
        NodeFilter: 'readonly',
      },
    },
    rules: {
      'no-unused-vars': ['error', { caughtErrorsIgnorePattern: '^_' }],
      'no-undef': 'error',
      'no-redeclare': 'error',
      'no-const-assign': 'error',
    },
  },
];
