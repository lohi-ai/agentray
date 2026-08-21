# Releasing the SDKs

Four packages ship from `sdk/`. They version independently — a fix to the Swift
client is not a reason to bump the browser one.

| Package | Registry | Source |
| --- | --- | --- |
| `@agentray/browser` | npm | `sdk/browser/` |
| `@agentray/server` | npm | `sdk/server/` |
| `agentray` | PyPI | `sdk/python/` |
| `AgentRay` (Swift) | Swift Package Manager — a git tag, no registry | `sdk/swift/` |

## Before the first publish

These are one-time account steps, not repo changes:

1. **Claim the npm scope.** `@agentray` must exist as an org (or a user scope)
   before `npm publish` will accept `@agentray/browser`. Both packages already
   set `publishConfig.access: public`, which is what stops a scoped publish from
   silently going private — or failing outright on a free account.
2. **Claim `agentray` on PyPI**, and generate a project-scoped API token.
3. Decide whether CI publishes. The workflow in `.github/workflows/sdk.yml`
   builds and tests only. Adding a publish job means putting `NPM_TOKEN` and
   `PYPI_TOKEN` in repository secrets; PyPI trusted publishing (OIDC) avoids the
   token entirely and is the better option if you go that way.

## Cutting a release

Every package's `prepublishOnly` runs typecheck → test → build, so a broken tree
cannot be published by hand. Run the checks anyway before you tag — a failure is
cheaper to read outside `npm publish`.

```bash
make sdk-check              # all four: typecheck, test, build, artefact contents
```

### npm packages

```bash
cd sdk/browser              # or sdk/server
npm version patch           # or minor / major — writes package.json + a git tag
npm publish                 # prepublishOnly re-runs the full gate
```

`npm version` creates a tag named `v<version>`, which collides between the two
packages. Retag per package before pushing:

```bash
git tag -d "v$(node -p "require('./package.json').version")"
git tag "browser-v$(node -p "require('./package.json').version")"
```

### Python

```bash
cd sdk/python
python -m build             # writes dist/*.whl and dist/*.tar.gz
twine check dist/*
twine upload dist/*
```

**Look inside the wheel before uploading**, every time:

```bash
unzip -l dist/*.whl | grep agentray/client.py
```

This is not a formality. The package built, passed `twine check`, and contained
no code at all until 2026-08-22: the repo-root `.gitignore` carries `/agentray`
for the compiled Go binary, and hatchling re-anchors that pattern to
`sdk/python/`, where it matches the Python package itself. `pyproject.toml` now
sets `ignore-vcs = true` with an explicit exclude list, and CI asserts on the
wheel's contents — but the failure mode was invisible at every step except
`import agentray`, so it is worth thirty seconds of your own eyes.

### Swift

SwiftPM resolves from git; there is nothing to upload.

```bash
cd sdk/swift && swift test
git tag swift-v0.1.0 && git push origin swift-v0.1.0
```

Consumers then point at the tag:

```swift
.package(url: "https://github.com/lohi-ai/agentray.git", from: "0.1.0")
```

Note that SwiftPM expects `Package.swift` at the **repository root** when
resolving by URL. Until the Swift client moves to its own repository (or a
`agentray-swift` mirror is published), the documented install stays the local
path dependency in `sdk/swift/README.md`.

## After publishing the browser package

`web/modules/start/components/instrument-snippet.tsx` currently emits a
hand-written inline snippet — a second implementation of the same event contract,
kept in sync by hand. Once `@agentray/browser` is on npm and served by unpkg, the
snippet should become a `<script src>` pointing at `dist/index.global.js`, so the
copy-paste path and the installed path are one tested bundle. Pin the major
version in the URL; an analytics tag that silently upgrades on someone else's
marketing site is a liability.

## Version policy

Pre-1.0, treat a change to any of the following as a **minor** bump, because each
one changes numbers a customer is already reading:

- what an event is named (`user.pageview`, `$autocapture`, `revenue`)
- what `platform` reports
- how identity is minted, aliased, or reset
- whether a failed delivery is retried, re-queued, or dropped

Everything else is a patch.
