# Releasing the SDKs

Four packages ship from `sdk/`. They version independently — a fix to the Swift
client is not a reason to bump the browser one.

| Package | Source | Primary host | Registry |
| --- | --- | --- | --- |
| `@agentray/browser` | `sdk/browser/` | GitHub Release (`.tgz` + `.min.js`) | npm |
| `@agentray/server` | `sdk/server/` | GitHub Release (`.tgz`) | npm |
| `agentray` (Python) | `sdk/python/` | GitHub Release (wheel + sdist) | PyPI |
| `AgentRay` (Swift) | `sdk/swift/` | `lohi-ai/agentray-swift` mirror | — (SwiftPM reads git tags) |

**GitHub is the primary host.** Every release lands as a GitHub Release with the
real artefact attached, which anyone can install with no registry account, no
scope claim and no auth. Publishing to npm/PyPI is a second step on the same tag,
and it is **skipped, not failed**, when its token is absent — so the pipeline is
live today and starts publishing to the registries the moment their secrets
exist. Nothing needs re-plumbing when that happens; just re-run the workflow on
the tag.

## Cutting a release

```bash
make sdk-release SDK_PKG=browser SDK_BUMP=minor    # patch | minor | major | x.y.z
```

That bumps the manifest, runs the package's full check, commits, and creates the
tag `browser-v<version>`. It deliberately **does not push**, because the tag push
is what publishes. When you are ready:

```bash
git push origin main browser-v0.2.0
```

Tags are `<browser|server|python|swift>-v<semver>`. A bare `v0.2.0` is a *product*
tag and is deliberately not matched by the release workflow — with four
independently versioned packages it would be ambiguous.

**Push release tags one at a time.** GitHub creates no workflow runs at all when
more than three tags arrive in a single push — no error, no run, the tags simply
land and nothing happens. All four v0.1.0 tags were pushed together on
2026-08-23 and published nothing; the recovery is the manual trigger below, with
*dry run* unchecked, which is why that input exists.

`sdk/scripts/resolve-tag.mjs` is the single parser for those tags, and it refuses
a tag whose version disagrees with the manifest it claims to release. It runs in
three places: when you cut the tag, on every PR that touches `sdk/`
(the `tag-contract` job), and as the first job of the release itself. Publishing
`0.2.0` from a tree that says `0.1.0` is not a mistake a registry lets you take
back.

## What the tag push does

`.github/workflows/sdk-release.yml`:

1. **Resolves the tag** → package, directory, version, prerelease, npm dist-tag.
   A prerelease (`browser-v0.2.0-rc.1`) is marked as one on GitHub and published
   under the `next` dist-tag, so it never becomes what `npm i` installs.
2. **Rebuilds and re-verifies the tagged tree** — the same typecheck, tests, and
   artefact assertions the PR gate runs, plus an install of the packed tarball
   into an empty project. A release gate weaker than the PR gate is not a gate.
3. **Creates the GitHub Release** with the artefacts attached and install
   instructions in the body.
4. **Publishes to the registry**, if its secret is set.

To exercise all of that without publishing anything, run the workflow manually:
**Actions → sdk-release → Run workflow**, give it a tag, leave *dry run* checked.

The same manual trigger, with *dry run* **unchecked**, is how you publish a tag
that is already pushed — after a run failed on something outside the artefact, or
after a batched tag push created no runs at all:

```bash
gh workflow run sdk-release.yml --ref browser-v0.1.0 -f tag=browser-v0.1.0 -f dry_run=false
```

## Secrets, and what is missing without them

| Secret | Enables | Without it |
| --- | --- | --- |
| `NPM_TOKEN` | `npm publish --provenance` for both npm packages | GitHub Release only; the job logs a notice |
| `PYPI_TOKEN` | `twine upload` of the wheel + sdist | GitHub Release only; the job logs a notice |
| `SWIFT_MIRROR_TOKEN` | pushing the `lohi-ai/agentray-swift` mirror + its tag | source tarball on the Release only, no SwiftPM install |

Re-running a tag is safe: each publish step checks whether that exact version is
already out (`npm view`, `twine --skip-existing`, a tag lookup on the mirror) and
no-ops if it is. A published SwiftPM tag is never moved — resolving a version
pins it to a commit, so re-pointing one breaks whoever already resolved it.

Three one-time account steps unblock the rest:

1. **Claim the `@agentray` npm scope** (it is unclaimed as of 2026-08-22), then
   add an automation token as `NPM_TOKEN`. Both packages already set
   `publishConfig.access: public`, which is what stops a scoped publish from
   silently going private or failing outright on a free account.
2. **Claim `agentray` on PyPI** (also unclaimed) and add a project-scoped token
   as `PYPI_TOKEN`. PyPI trusted publishing (OIDC) avoids the long-lived token
   entirely and is the better option if you are willing to configure it on the
   PyPI side; the workflow already requests `id-token: write`.
3. `lohi-ai/agentray-swift` **exists and publishes 0.1.0** — pushed on 2026-08-23
   by running the workflow's own steps by hand, because CI had no token. Add a
   PAT with write access to that repo as `SWIFT_MIRROR_TOKEN`, or every later
   Swift release ships the tarball and no SwiftPM version. `GITHUB_TOKEN` cannot
   be used — it is scoped to this repository only.

Note that GitHub *Packages* (`npm.pkg.github.com`) is **not** the GitHub host
here, and cannot be. Its npm registry requires the package scope to equal the
repository owner, so under `lohi-ai` it would only accept `@lohi-ai/*` — the
`agentray` GitHub name is taken by an unrelated user, so an `@agentray` org is
not available either. It also requires consumers to authenticate with a PAT even
for public packages. GitHub Releases keeps one name across both hosts and needs
no auth to install from.

## Verifying by hand

Every package's `prepublishOnly` runs typecheck → test → build, so a broken tree
cannot be published by hand either. Run the checks anyway before you tag — a
failure is cheaper to read outside `npm publish`:

```bash
make sdk-check              # all four: typecheck, test, build, artefact contents
```

The artefact assertions live in `sdk/scripts/` precisely so that the Makefile,
the PR gate and the release gate check the same things:

| Script | Asserts |
| --- | --- |
| `verify-npm-tarball.mjs` | every entrypoint `package.json` declares is actually in the tarball |
| `verify-consumer-install.mjs` | the tarball installs into an empty project and imports as ESM, as CJS, and in types |
| `verify-cdn-bundle.mjs` | `dist/index.global.js` attaches `window.AgentRay` and gets an event to the wire |
| `verify-python-wheel.py` | the wheel contains the package, not just metadata |
| `resolve-tag.mjs` | the tag names a real package at the version the tree claims |

**Look inside the wheel before uploading**, every time. This is not a formality.
The Python package built, passed `twine check`, and contained no code at all
until 2026-08-22: the repo-root `.gitignore` carries `/agentray` for the compiled
Go binary, and hatchling re-anchors that pattern to `sdk/python/`, where it
matches the Python package itself. `pyproject.toml` now sets `ignore-vcs = true`
with an explicit exclude list, and both CI and `make sdk-check` assert on the
wheel's contents — but the failure mode was invisible at every step except
`import agentray`, so it is worth thirty seconds of your own eyes.

## After the browser package reaches a registry

`web/modules/start/components/instrument-snippet.tsx` currently emits a
hand-written inline snippet — a second implementation of the same event contract,
kept in sync by hand. Once `@agentray/browser` is on npm and served by unpkg, the
snippet should become a `<script src>` pointing at `dist/index.global.js`, so the
copy-paste path and the installed path are one tested bundle. Pin the major
version in the URL; an analytics tag that silently upgrades on someone else's
marketing site is a liability.

Until then the release attaches `agentray-browser-<version>.min.js` as its own
asset, so the bundle is at least downloadable and self-hostable.

## Version policy

Pre-1.0, treat a change to any of the following as a **minor** bump, because each
one changes numbers a customer is already reading:

- what an event is named (`user.pageview`, `$autocapture`, `revenue`)
- what `platform` reports
- how identity is minted, aliased, or reset
- whether a failed delivery is retried, re-queued, or dropped

Everything else is a patch.
