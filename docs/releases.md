# Release model

PUA Server, AgentHub, and PUA.app share one release train. Every release uses a
single stable version and annotated tag such as `v0.3.0`, and creates one GitHub
Release. A release may contain any non-empty subset of the three components.

`versions/release` contains the current train version without the `v` prefix.
`versions/pua`, `versions/agenthub`, and `versions/macapp` contain the most recent
train each component joined. A component joins the current release only when its
version file equals `versions/release`; skipped components retain their previous
version and may jump over global versions later.

Run `scripts/release-plan vX.Y.Z` before tagging. It rejects malformed versions,
tags without the required `v` prefix, tags that do not match `versions/release`,
component versions ahead of the train, and releases with no selected component.
The release workflow performs the same check before it builds anything.

## Artifacts and update feeds

One workflow run builds the selected components, uploads temporary Actions
artifacts, and creates the GitHub Release once after every selected build succeeds.
Release metadata files have component-qualified names so assets from the same
release cannot overwrite one another:

- `pua-release.json`, `pua-SHA256SUMS`, and `pua-notarization.json`;
- `agenthub-release.json`, `agenthub-SHA256SUMS`, and
  `agenthub-notarization.json`;
- `macapp-appcast.xml`, `macapp-SHA256SUMS`, and
  `macapp-notarization.json`.

After publishing, `scripts/build-release-site` copies the complete static website
and rebuilds the stable feeds from release history. It independently finds the
newest stable `vX.Y.Z` release containing each descriptor, so PUA and AgentHub do
not need to have joined the same train. GitHub Pages publishes the combined site:

```text
https://disksing.github.io/pua/
https://disksing.github.io/pua/updates/channel-v1.json
https://disksing.github.io/pua/updates/channel-v1.json.sig
https://disksing.github.io/pua/updates/appcast.xml
```

The component feed is generated only after stable PUA and AgentHub descriptors
both exist. The Sparkle feed is copied from the newest release containing a macOS
App appcast. These `/updates/` URLs are release API and must remain stable.

Before enabling automatic website deployments from `master`, configure Pages to
use GitHub Actions and set the repository Actions variable
`PUA_PAGES_ENABLED=true`. Until then, ordinary website-source pushes skip the
deployment job instead of failing. A release tag and a manual workflow dispatch
still request deployment so release/setup errors remain visible.

## Keys and compatibility

Generate the component manifest key once with:

```sh
go run ./cmd/component-manifest -generate-key
```

Store only the public half in `COMPONENT_UPDATE_PUBLIC_KEY`; store the private
half as the `COMPONENT_MANIFEST_PRIVATE_KEY` Actions secret. Sparkle uses its own
keypair. Generate it with Sparkle's `generate_keys`, put its public half in
`SPARKLE_PUBLIC_KEY`, and store its private key as `SPARKLE_PRIVATE_KEY` (a local
release may instead use `SPARKLE_KEY_ACCOUNT`). Never commit either private key.

Compatibility floors are reviewed files, not inferred from the newest bundled
component: `versions/pua-min-agenthub`,
`versions/pua-min-desktop-manager`, and
`versions/agenthub-min-desktop-manager`. Change a floor only when a component
adopts that dependency or manager protocol. `agenthub/protocol.BaselineV1` is the
fixed API-v1 safety baseline and must not grow with optional capabilities.

Stable builds use the selected component's plain `X.Y.Z`. All local components
use `versions/release` as the common development baseline and report
`X.Y.Z-dev.<commit-count>+g<sha>`. The `PUA Dev` bundle remains isolated from the
stable App's bundle identifier, Application Support directory, CLI directory,
AgentHub home, and ports.

## Local release rehearsal

On a clean macOS checkout, `scripts/release-local [OUTPUT_DIR]` reads the same
release plan and invokes the production component/App builders for the selected
components. The default output is `dist/vX.Y.Z/`. It requires the same Developer
ID, notarization, manifest, and Sparkle credentials as GitHub Actions.

Use `scripts/test-release-local` to test version and selection behavior without
signing or notarization. Formal releases are created only by pushing an annotated
`vX.Y.Z` tag whose commit already exists on the remote `master` branch.
