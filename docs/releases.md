# Release model

PUA.app, PUA Server, and AgentHub have independent SemVer streams and tags:

- `macapp-vX.Y.Z` publishes a signed, notarized Universal DMG and Sparkle appcast.
- `pua-vX.Y.Z` publishes four signed/checksummed component binaries plus `release.json`.
- `agenthub-vX.Y.Z` publishes the corresponding independent AgentHub assets.

The latest stable PUA and AgentHub descriptors are combined into `channel-v1.json`, signed with a dedicated Ed25519 key, and deployed with its `.sig` to GitHub Pages. Generate that key once with:

```sh
go run ./cmd/component-manifest -generate-key
```

Store only the public half in `COMPONENT_UPDATE_PUBLIC_KEY`; store the private half as the `COMPONENT_MANIFEST_PRIVATE_KEY` Actions secret. Sparkle has its own keypair and appcast. Generate it with Sparkle's `generate_keys`, put its public half in `SPARKLE_PUBLIC_KEY`, and export the private key as the `SPARKLE_PRIVATE_KEY` Actions secret (a local release may instead use `SPARKLE_KEY_ACCOUNT`).

Stable builds use plain `X.Y.Z`. Local builds use `X.Y.Z-dev.<commit-count>+g<sha>` and the `PUA Dev` bundle, which has a separate bundle identifier, Application Support directory, CLI directory, AgentHub home, and ports. Component manifest schema/API major and the desktop manager protocol are independent compatibility axes; `agenthub/protocol.BaselineV1` is the fixed API-v1 safety baseline and must not grow.

Compatibility floors are reviewed files, not inferred from the newest bundled component: `versions/pua-min-agenthub`, `versions/pua-min-desktop-manager`, and `versions/agenthub-min-desktop-manager`. A component release changes one of these only when it actually adopts an incompatible dependency or manager protocol.
