Vendored web test fixtures

axe-core.min.js.txt

Vendored axe-core axe.min.js used by the T41 UI qualification spec
(ui-qualification.spec.ts).

- Version: 4.13.0 (npm registry tarball axe-core 4.13.0)
- SHA-256: c24f097bd2f451d4f933e8bc7d8d539f8672a2ebcb5ccf9f3eec8ca9470a0c1
- Role: injected by the spec into dedicated audit contexts
  (bypassCSP: true) that exist only for the accessibility scan; every
  behavioral assertion runs in ordinary CSP-enforced contexts.
- The .txt suffix keeps eslint from parsing the minified bundle while
  readFileSync loads it as plain text for page.addScriptTag.
