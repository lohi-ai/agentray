# Releasing the SDKs

Three packages ship from this repository. They version independently — a fix to
the server client is not a reason to bump the browser one.

| Package | Source | Primary host | Registry |
| --- | --- | --- | --- |
| `@agentray/browser` | `sdk/browser/` | GitHub Release (`.tgz` + `.min.js`) | npm |
| `@agentray/server` | `sdk/server/` | GitHub Release (`.tgz`) | npm |
| `agentray` (Python) | `sdk/python/` | GitHub Release (wheel + sdist) | PyPI |

**The Swift SDK is released elsewhere.** It is its own repository,
[lohi-ai/agentray-swift](https://github.com/lohi-ai/agentray-swift), carried here
as a submodule at `sdk/swift/`, and it builds, tests and releases itself. Nothing
in this runbook applies to it — see [Swift](#swift) at the bottom.

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

That checks the package, bumps and commits its manifest/lockfile, and validates
its package-specific version. It does not create a local tag or push. Write the
new version's changelog first, then publish the reviewed source change:

```bash
git push origin main
```

Tags remain `<browser|server|python>-v<semver>`. SDK versions are independent;
a bare product `v…` tag does not release an SDK. A prerelease is marked on
GitHub and uses npm's `next` dist-tag.

## One verification and release run

`.github/workflows/sdk-release.yml` owns main and SDK tag pushes:

1. Read each manifest and npm lockfile, reject version drift/downgrades, and
   select only versions without a completed release. Never invent a version.
2. Call `sdk.yml` once for the exact commit. It typechecks/tests/builds, inspects
   tarball/wheel contents, verifies clean consumers, and tests the browser global
   in the same build. PRs call the same read-only checks.
3. After all checks pass, create immutable tags and download the verified
   tarballs, browser bundle, wheel and sdist from that run. No second build or
   publish hook runs.
4. Add versioned documentation, `DOWNLOADS.md` and checksums. Attach every asset
   to a draft, then publish the complete GitHub Release and optionally npm/PyPI.

Main pushes without a version bump still run checks and skip release jobs.
GitHub's bot-created tags do not launch another run: distribution is part of the
original main workflow. Human tag pushes use the same pipeline.

For recovery or verification, dispatch the current workflow on `main` with an
existing tag. Leave `dry_run=true` to verify without publishing:

```bash
gh workflow run sdk-release.yml --ref main -f tag=browser-v0.2.0 -f dry_run=false
```

An unpublished tag/draft can resume only at its original commit. A published
release's files are immutable; an explicit registry retry downloads those
original assets instead of replacing them with a rebuild. This also supports
legacy releases that predate checksum manifests. Never move a published tag.

## Secrets, and what is missing without them

| Secret | Enables | Without it |
| --- | --- | --- |
| `NPM_TOKEN` | `npm publish --provenance` for both npm packages | GitHub Release only; the job logs a notice |
| `PYPI_TOKEN` | `twine upload` of the wheel + sdist | GitHub Release only; the job logs a notice |

Re-running a tag is safe: each publish step checks whether that exact version is
already out (`npm view`, `twine --skip-existing`) and no-ops if it is. Never
move a tag that has already published — a registry does not let you take a
version back, and a resolved SwiftPM version pins to a commit.

Two one-time account steps unblock both:

1. **Claim the `@agentray` npm scope** (it is unclaimed as of 2026-08-22), then
   add an automation token as `NPM_TOKEN`. Both packages already set
   `publishConfig.access: public`, which is what stops a scoped publish from
   silently going private or failing outright on a free account.
2. **Claim `agentray` on PyPI** (also unclaimed) and add a project-scoped token
   as `PYPI_TOKEN`. PyPI trusted publishing (OIDC) avoids the long-lived token
   entirely and is the better option if you are willing to configure it on the
   PyPI side; the workflow already requests `id-token: write`.

There is deliberately no third secret. The Swift SDK used to be pushed from here
into `lohi-ai/agentray-swift`, which needed a credential with write access to
another repository — deploy keys are disabled org-wide, `GITHUB_TOKEN` cannot
reach another repo, and a PAT broad enough to push there is broad enough to push
anywhere the account can. Moving that CI into the repo it writes to removed the
credential instead of managing it.

Note that GitHub *Packages* (`npm.pkg.github.com`) is **not** the GitHub host
here, and cannot be. Its npm registry requires the package scope to equal the
repository owner, so under `lohi-ai` it would only accept `@lohi-ai/*` — the
`agentray` GitHub name is taken by an unrelated user, so an `@agentray` org is
not available either. It also requires consumers to authenticate with a PAT even
for public packages. GitHub Releases keeps one name across both hosts and needs
no auth to install from.

## Verifying by hand

Every package's `prepublishOnly` runs typecheck → test → build, so a broken tree
cannot be published by hand either. Run the checks anyway before pushing the version change — a
failure is cheaper to read outside `npm publish`:

```bash
make sdk-check              # all four: typecheck, test, build, artefact contents
```

The artefact assertions live in `sdk/scripts/` precisely so that the Makefile,
the PR gate and the release gate check the same things:

| Script | Asserts |
| --- | --- |
| `verify-npm-tarball.mjs` | every entrypoint `package.json` declares, plus `README.md`, `LICENSE` and `CHANGELOG.md`, is actually in the tarball — and that the changelog mentions the version being published |
| `verify-consumer-install.mjs` | the tarball installs into an empty project and imports as ESM, as CJS, and in types, including the standard money constants at their contract values |
| `verify-cdn-bundle.mjs` | `dist/index.global.js` attaches `window.AgentRay` and gets an event to the wire |
| `verify-python-wheel.py` | the wheel contains the package, not just metadata |
| `resolve-tag.mjs` | the tag names a real package at the version the tree claims |
| `check-workflow-shell.py` | every workflow `run` block parses under bash 3.2, the macOS runner's shell |

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

## Swift

`sdk/swift/` is a submodule of
[lohi-ai/agentray-swift](https://github.com/lohi-ai/agentray-swift). That repo is
the source of record; this one pins a commit of it.

SwiftPM reads `Package.swift` from a repository **root** and clones the whole
repository to do it. Pointed at this monorepo it fails outright —
`the package manifest at '/Package.swift' cannot be accessed` — and it reads this
repo's product tags (`v0.1.0` … `v0.2.0`) as AgentRay versions, so `from: "0.1.0"`
would resolve a commit containing no Swift package at all. Hence a separate repo,
and hence its CI lives there, where the built-in `GITHUB_TOKEN` can write.

To change the Swift client:

```bash
git submodule update --init sdk/swift     # if the directory is empty
cd sdk/swift
# edit, then commit and push in the submodule — its CI builds and tests it
cd ../.. && git add sdk/swift && git commit -m 'bump swift SDK'
```

To release Swift, update `VERSION` in that repository and push `main`. Its
workflow builds/tests once, creates the **bare SemVer** tag SwiftPM reads,
resolves a consumer by URL, and publishes source/docs/download instructions.
An unchanged version still checks without creating another tag. PR checks and
release checks call the same read-only workflow. See its README for recovery.

## Version policy

Pre-1.0, treat a change to any of the following as a **minor** bump, because each
one changes numbers a customer is already reading:

- what an event is named (`user.pageview`, `$autocapture`, `revenue`,
  `revenue_reversed`)
- what a money event's `amount` unit is, which property a row must carry, or
  which key makes it de-dupable
- what `platform` reports
- how identity is minted, aliased, or reset
- whether a failed delivery is retried, re-queued, or dropped

Everything else is a patch.

## Changelogs

Every npm package carries a package-local `CHANGELOG.md`, listed in its `files`
so it ships inside the tarball. Write the entry **as part of the change**, not at
release time: the migration note a customer needs is the one you can still
remember writing.

The entry for a version must name that version and be written for the person
upgrading — what changed, what now fails to compile, and what value they have to
correct in their own code. `verify-npm-tarball.mjs` fails the PR gate and the
release gate when the changelog does not mention the version being published, so
a bumped manifest with last release's entry cannot reach a registry.

Pre-1.0 there is no deprecation window: a breaking change is allowed, and the
changelog is where it is paid for. `sdk/browser/CHANGELOG.md` and
`sdk/server/CHANGELOG.md` are the reference for the shape.
