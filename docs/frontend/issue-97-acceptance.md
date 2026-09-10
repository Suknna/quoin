# #97 frontend acceptance evidence

Date: 2026-09-10. Baseline: `5547ea0`.

## Evidence boundaries

The main agent used the interactive browser against the explicitly labelled mock preview at `http://127.0.0.1:5197`. These observations establish rendering and navigation, not production integration. Separately, the supported production topology at `https://localhost:8459` passed the real #97 test and the combined #96/#97/#102 suite (3/3). See [functional acceptance](../issue-97-validation.md).

## Five required checks

1. **Title/content/action hierarchy:** inspected the Prometheus configuration form and business declaration editor at 1920×1080 CSS pixels. The forms retain one Card and separate rule sections with whitespace and Separators. Primary create/verify actions and return navigation are labelled. The configuration form has a parallel instructions column.
2. **Discoverable actions:** used the integration catalog, business New button, YAML Apply, typed business-name field, missing-integration link, and Return to business draft. Business rows now use semantic Item/button composition with icons, hover states, status badges, and chevrons. Metrics rows expose a labelled Manage action with a chevron. A 50-row inspection initially revealed clipped actions; fixed table columns and truncation were applied and visually rechecked.
3. **0/1/50/long-text boundaries:** inspected the default one-business state, empty business state, and the visible `business-boundary` scenario containing 50 businesses including a long name. Inspected `metrics-boundary` with 50 metric integrations including a long name; its final row retains the visible Manage action and a truncated name. Empty declaration state shows missing metrics/Label Contract guidance. Arbitrary 50-rule declaration documents and every modal permutation were not exhaustively inspected.
4. **1920px width:** metrics forms retain a bounded two-column layout; the business editor uses `max-w-5xl`. Integration tables use fixed column proportions, with long names/endpoints truncated and full values in title attributes. The final 50-item table screenshot shows status, endpoint, and actions within the content width.
5. **Peer headings:** reviewed form section and instructions headings in the inspected screens. Declaration rule groups reuse FieldLabel and Separator rather than introducing nested Cards. Existing unrelated browser-identity UI was not redesigned.

Additional responsive observation: the business editor was checked at 390×844 CSS pixels. An initial document-level horizontal scrollbar was repaired by zero-minimum grid tracks and minimum-width constraints; the subsequent screenshot has no horizontal document scrollbar. A narrow metrics data table intentionally retains local horizontal scrolling rather than hiding columns.

## Interaction results

- Pasting a complete canonical YAML declaration disables typed editing until Apply, then restores name, identity rules, alert conditions and PromQL fields.
- Editing the business name preserves the query and identity declarations.
- An imported unavailable metrics ID is shown explicitly as an unavailable reference, never replaced with the first integration.
- In the empty scenario, entered `browser-return-final97`, followed Create Prometheus, then Return to business draft. The incomplete non-secret business identifier was restored exactly.
- Long business-row sizing and native button semantics were corrected after initial browser inspection. Automated component tests supplement those observations; they are not a substitute for the screenshots.

## Screenshots from the actual interactive browser

Screenshots are local session artifacts, not committed generated files. The full-page capture API produced an incorrectly tiled image once; that image was rejected and viewport captures were used instead.

- [Prometheus form, 1920px](/home/suknna/.zcode/cli/artifacts/sess_3b106980-73e0-4794-abff-18f3b2279caa/call_UbyCRwmIfzUOgxKEcBTFe6BS-tool-result-b9dbbeab-52f0-424c-8757-c153c45bba94.png)
- [Declaration form with rules, 1920px](/home/suknna/.zcode/cli/artifacts/sess_3b106980-73e0-4794-abff-18f3b2279caa/call_6hL0ktk8ybsNGrzGEjgyd0Ey-tool-result-900e1d5c-b8c3-4d39-85a9-894ff2fcd6be.png)
- [Corrected narrow editor, 390px](/home/suknna/.zcode/cli/artifacts/sess_3b106980-73e0-4794-abff-18f3b2279caa/call_forhg3nJkW3RsCXFbVWNrSrP-tool-result-cb0925ee-33ca-4176-b82e-564c687ae0c7.png)
- [Corrected metrics table, 50 items and long name, 1920px](/home/suknna/.zcode/cli/artifacts/sess_3b106980-73e0-4794-abff-18f3b2279caa/call_SVD0i9gDmAOs1mEqaQ87O59w-tool-result-e1add171-6031-4a20-af82-a106a4e0eae7.png)
