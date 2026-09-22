## Agent skills

### Issue tracker

Issues and specs are tracked in GitHub Issues via the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

The default five-role triage label vocabulary is used. See `docs/agents/triage-labels.md`.

### Domain docs

This repository uses the single-context domain documentation layout. See `docs/agents/domain.md`.

### Releases

When preparing or publishing a release, follow `docs/releasing.md`. Release tags use
`vX.Y.Z` SemVer. Before `v1.0.0`, keep X at `0`; Y may include incompatible changes
and Z is a fix. From `v1.0.0`, X marks incompatible stable releases, Y marks
backwards-compatible features, and Z marks backwards-compatible fixes. The first
public release is `v0.1.0`.
