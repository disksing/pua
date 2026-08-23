// @ts-nocheck -- pure model-choice logic migrated unchanged and covered by tests.
// Pure logic behind the agent model dropdown, shared by the Svelte component
// and node --test unit tests. The dropdown never accepts free text: every
// choice comes from the provider's live model list (GET
// /v1/providers/{id}/models), except a previously saved value that is no
// longer listed, which is preserved explicitly instead of being dropped.

export const PROVIDER_DEFAULT_VALUE = "";

// withUpstreamPrefix prefixes a model label with the upstream provider parsed
// from a "provider/model" ID (pi, kimi and opencode enumerate several
// upstreams under one AgentHub provider, and their bare model names often
// collide). IDs without a slash (e.g. codex) and labels that already mention
// the provider are left untouched; the mention check ignores case and
// punctuation so "Kimi For Coding/..." counts as mentioning "kimi-for-coding".
function withUpstreamPrefix(id, label) {
  const slash = id.indexOf("/");
  if (slash <= 0) return label;
  const compact = (text) => text.toLowerCase().replace(/[^a-z0-9]/g, "");
  const provider = id.slice(0, slash);
  if (compact(label).includes(compact(provider))) return label;
  return `${provider} / ${label}`;
}

// buildModelChoices turns the daemon's model list into select options. The
// first option is always the empty "Provider default" choice (the agent
// simply omits the model option). A saved value that is missing from the
// list is appended as an explicit unavailable option so editing an old agent
// never silently rewrites or clears its model.
//
// Model IDs from multi-upstream providers (pi, kimi, opencode) look like
// "upstream/model", while the daemon's label is only the model name, so
// identical names from different upstreams would be indistinguishable. The
// upstream prefix is shown as "upstream / label" unless the label already
// mentions it.
export function buildModelChoices(models, current) {
  const choices = [{ value: PROVIDER_DEFAULT_VALUE, label: "Provider default", unavailable: false }];
  const seen = new Set();
  for (const model of Array.isArray(models) ? models : []) {
    const value = String(model?.id ?? "").trim();
    if (!value || seen.has(value)) continue;
    seen.add(value);
    const label = String(model?.label ?? "").trim() || value;
    const display = withUpstreamPrefix(value, label);
    choices.push({
      value,
      label: model?.default ? `${display} (default)` : display,
      unavailable: false,
    });
  }
  const saved = String(current ?? "").trim();
  if (saved && !seen.has(saved)) {
    choices.push({
      value: saved,
      label: `${saved} (saved, not currently listed)`,
      unavailable: true,
    });
  }
  return choices;
}

// modelListView reduces the fetch state and the saved value into everything
// the component needs to render: the select options, whether the select is
// interactive, and an optional status message. States: "loading", "ready",
// "error", "disabled" (provider toggled off), "none" (no provider selected).
export function modelListView(state, current) {
  const saved = String(current ?? "").trim();
  switch (state.status) {
    case "ready":
      return {
        disabled: false,
        choices: buildModelChoices(state.models, saved),
        message: state.models?.length ? "" : "This provider did not report any models; the provider default will be used.",
        tone: state.models?.length ? "" : "muted",
      };
    case "loading":
      return {
        disabled: true,
        choices: [{ value: saved, label: "Loading models…", unavailable: false }],
        message: "Loading the provider's model list…",
        tone: "muted",
      };
    case "error":
      return {
        disabled: true,
        choices: [{ value: saved, label: saved || "Provider default", unavailable: false }],
        message: state.error || "Failed to load the model list.",
        tone: "error",
        retry: true,
      };
    case "disabled":
      return {
        disabled: true,
        choices: [{ value: saved, label: saved || "Provider default", unavailable: false }],
        message: "This provider is disabled; its model list is unavailable.",
        tone: "muted",
      };
    default:
      return {
        disabled: true,
        choices: [{ value: saved, label: saved || "Provider default", unavailable: false }],
        message: "Select a provider to load its models.",
        tone: "muted",
      };
  }
}
