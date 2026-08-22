import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { CheckCircle, Warning, X } from "@phosphor-icons/react";
import { api } from "../api";
import { buildPayload, canSaveDraft, createDraft, isDirty, normalizeConfig, validateDraft } from "./configModel";
import { applyProviderToggle, requestProviderToggle } from "./providerSwitches";
import { ProvidersPanel } from "./ProvidersPanel";
import { AgentsPanel } from "./AgentsPanel";
import { GeneralPanel } from "./GeneralPanel.jsx";
import { ActivityPanel } from "./ActivityPanel.jsx";
import { loadSettingsAuxiliary, loadSettingsConfig } from "./loadSettings.js";
import {
  companionPreferencesEqual,
  loadCompanionPreferences,
  saveCompanionPreferences,
  validateCompanionPreferences,
} from "../companion/preferences.js";

const SECTIONS = [
  { id: "general", label: "General" },
  { id: "activity", label: "Activity" },
  { id: "providers", label: "Providers" },
  { id: "agents", label: "Agents" },
];

// The draft only contains JSON-safe data (all of it produced by
// normalizeConfig), so a JSON round trip is a safe deep copy.
function clone(value) {
  return JSON.parse(JSON.stringify(value));
}

export function SettingsModal({ onClose, onSaved, triggerRef }) {
  const [phase, setPhase] = useState("loading");
  const [loadError, setLoadError] = useState("");
  const [draft, setDraft] = useState(null);
  const [snapshot, setSnapshot] = useState(null);
  const [activityDraft, setActivityDraft] = useState(null);
  const [activitySnapshot, setActivitySnapshot] = useState(null);
  const [probes, setProbes] = useState([]);
  const [quota, setQuota] = useState({ providers: [] });
  const [section, setSection] = useState("general");
  const [showErrors, setShowErrors] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [conflict, setConflict] = useState(false);
  const [savedOk, setSavedOk] = useState(false);
  // Provider switches persist immediately (not through the draft/save-all
  // flow): pendingProviderId serializes toggles and prevents double submits.
  const [pendingProviderId, setPendingProviderId] = useState(null);
  const [providerToggleError, setProviderToggleError] = useState("");
  const dialogRef = useRef(null);
  const savedTimer = useRef(null);
  const loadGeneration = useRef(0);

  const serverDirty = draft && snapshot ? isDirty(draft, snapshot) : false;
  const activityDirty = activityDraft && activitySnapshot
    ? !companionPreferencesEqual(activityDraft, activitySnapshot)
    : false;
  const dirty = serverDirty || activityDirty;
  const errors = useMemo(() => (
    draft && activityDraft
      ? [...validateDraft(draft), ...validateCompanionPreferences(activityDraft)]
      : []
  ), [draft, activityDraft]);
  const displayErrors = showErrors || errors.length > 0;

  const load = useCallback(async () => {
    const generation = ++loadGeneration.current;
    setPhase("loading");
    setLoadError("");
    setConflict(false);
    setSaveError("");
    setShowErrors(false);
    setProbes([]);
    setQuota({ providers: [] });
    try {
      const configBody = await loadSettingsConfig(api);
      if (generation !== loadGeneration.current) return;
      const next = createDraft(configBody.config || {});
      const nextActivity = loadCompanionPreferences();
      setDraft(next);
      setSnapshot(clone(next));
      setActivityDraft(nextActivity);
      setActivitySnapshot(clone(nextActivity));
      setPhase("ready");

      // Do not make the usable configuration wait for provider probes or
      // quota. Each auxiliary request has its own deadline and stale loads
      // cannot overwrite a newer retry or a remounted dialog.
      loadSettingsAuxiliary(api).then(({ agentsBody, quotaBody }) => {
        if (generation !== loadGeneration.current) return;
        setProbes(agentsBody.probes || []);
        setQuota(quotaBody.quota || { providers: [] });
      }).catch(() => {
        // loadSettingsAuxiliary settles individual failures; this guard keeps
        // an unexpected implementation error from replacing usable settings.
      });
    } catch (value) {
      if (generation !== loadGeneration.current) return;
      setLoadError(value.message || "Failed to load the configuration");
      setPhase("error");
    }
  }, []);

  useEffect(() => {
    load();
    return () => { loadGeneration.current += 1; };
  }, [load]);

  // Move focus into the dialog on open; restore it to the trigger button on
  // unmount.
  useEffect(() => {
    dialogRef.current?.focus();
    return () => {
      clearTimeout(savedTimer.current);
      triggerRef?.current?.focus();
    };
  }, [triggerRef]);

  const requestClose = useCallback(() => {
    if (dirty && !window.confirm("You have unsaved changes. Close settings and discard them?")) return;
    onClose();
  }, [dirty, onClose]);

  useEffect(() => {
    const onKeyDown = (event) => {
      if (event.key === "Escape") requestClose();
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [requestClose]);

  const mutate = useCallback((recipe) => {
    setSaveError("");
    setSavedOk(false);
    setDraft((current) => {
      const next = clone(current);
      recipe(next);
      return next;
    });
  }, []);

  const mutateActivity = useCallback((recipe) => {
    setSaveError("");
    setSavedOk(false);
    setActivityDraft((current) => {
      const next = clone(current);
      recipe(next);
      return next;
    });
  }, []);

  const updateProviderCommand = useCallback((id, command) => {
    mutate((next) => {
      const provider = next.agentProviders.find((item) => item.id === id);
      if (!provider) return;
      const normalized = String(command ?? "").trim();
      if (normalized) provider.command = normalized;
      else delete provider.command;
    });
  }, [mutate]);

  // toggleProvider flips one built-in provider through the minimal daemon
  // endpoint. On success the persisted provider is merged into both the draft
  // and the snapshot, so unsaved agent edits survive and dirty tracking stays
  // accurate. On failure the switches are re-aligned with the true persisted
  // state — the UI never shows a provider as enabled when it is not saved.
  const toggleProvider = useCallback(async (id, enabled) => {
    if (pendingProviderId || !draft) return;
    setPendingProviderId(id);
    setProviderToggleError("");
    const draftProvider = draft.agentProviders.find((item) => item.id === id);
    const snapshotProvider = snapshot?.agentProviders?.find((item) => item.id === id);
    const commandDirty = draftProvider?.command !== snapshotProvider?.command;
    const draftCommand = draftProvider?.command;
    try {
      const provider = await requestProviderToggle(api, id, enabled);
      setDraft((current) => {
        const next = applyProviderToggle(current, provider);
        if (commandDirty) {
          const updated = next.agentProviders.find((item) => item.id === id);
          if (updated) {
            if (draftCommand) updated.command = draftCommand;
            else delete updated.command;
          }
        }
        return next;
      });
      setSnapshot((current) => applyProviderToggle(current, provider));
      const agentsBody = await api("/v1/agents");
      setProbes(agentsBody.probes || []);
      onSaved?.();
    } catch (value) {
      setProviderToggleError(value.message || "Failed to update the provider");
      try {
        const configBody = await api("/v1/config");
        const fresh = createDraft(configBody.config || {});
        setDraft((current) => ({ ...current, agentProviders: fresh.agentProviders }));
        setSnapshot((current) => ({ ...current, agentProviders: fresh.agentProviders }));
      } catch {
        // Keep the last known state when the reload also fails.
      }
    } finally {
      setPendingProviderId(null);
    }
  }, [pendingProviderId, draft, snapshot, onSaved]);

  const save = async (force = false) => {
    if (saving || !draft) return;
    if (errors.length) {
      setShowErrors(true);
      setSection(errors[0].section);
      return;
    }
    setSaving(true);
    setSaveError("");
    try {
      if (serverDirty && !force) {
        // Concurrency guard: make sure the server-side config has not been
        // changed by another client before saving.
        const current = await api("/v1/config");
        if (JSON.stringify(normalizeConfig(current.config)) !== JSON.stringify(normalizeConfig(snapshot))) {
          setConflict(true);
          return;
        }
      }
      if (activityDirty) {
        const savedActivity = saveCompanionPreferences(activityDraft);
        setActivityDraft(savedActivity);
        setActivitySnapshot(clone(savedActivity));
      }
      if (serverDirty) {
        const payload = buildPayload(draft);
        await api("/v1/config", { method: "PUT", body: JSON.stringify({ config: payload }) });
        const agentsBody = await api("/v1/agents");
        setProbes(agentsBody.probes || []);
        setSnapshot(clone(payload));
      }
      setShowErrors(false);
      setConflict(false);
      setSavedOk(true);
      clearTimeout(savedTimer.current);
      savedTimer.current = setTimeout(() => setSavedOk(false), 3000);
      onSaved?.();
    } catch (value) {
      setSaveError(value.message || "Failed to save");
    } finally {
      setSaving(false);
    }
  };

  const sectionErrorCount = (id) => errors.filter((item) => item.section === id).length;

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) requestClose();
      }}
    >
      <section
        className="settings-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="settings-dialog-title"
        ref={dialogRef}
        tabIndex={-1}
      >
        <header className="settings-dialog-header">
          <div>
            <h2 id="settings-dialog-title">Settings</h2>
            <p>Configure the companion, providers, and agents. Changes take effect when saved.</p>
          </div>
          <button className="icon-button" aria-label="Close settings" onClick={requestClose}>
            <X size={19} />
          </button>
        </header>

        {phase === "loading" ? <div className="settings-status">Loading configuration…</div> : null}
        {phase === "error" ? (
          <div className="settings-status">
            <p className="settings-load-error" role="alert">{loadError}</p>
            <button className="settings-button" onClick={load}>Retry</button>
          </div>
        ) : null}

        {phase === "ready" ? (
          <div className="settings-body">
            <nav className="settings-nav" aria-label="Settings sections">
              {SECTIONS.map((item) => {
                const count = sectionErrorCount(item.id);
                return (
                  <button
                    key={item.id}
                    className={`settings-nav-button ${section === item.id ? "active" : ""}`}
                    aria-current={section === item.id ? "true" : undefined}
                    onClick={() => setSection(item.id)}
                  >
                    <span>{item.label}</span>
                    {displayErrors && count ? <span className="settings-nav-badge">{count}</span> : null}
                  </button>
                );
              })}
            </nav>
            <div className="settings-content">
              <div className="settings-panel">
                {section === "general" ? (
                  <GeneralPanel draft={draft} errors={errors} showErrors={displayErrors} mutate={mutate} />
                ) : null}
                {section === "activity" ? (
                  <ActivityPanel value={activityDraft} mutate={mutateActivity} quota={quota} />
                ) : null}
                {section === "providers" ? (
                  <ProvidersPanel
                    config={draft}
                    probes={probes}
                    pendingId={pendingProviderId}
                    toggleError={providerToggleError}
                    onToggle={toggleProvider}
                    onCommandChange={updateProviderCommand}
                  />
                ) : null}
                {section === "agents" ? (
                  <AgentsPanel draft={draft} probes={probes} errors={errors} showErrors={displayErrors} mutate={mutate} />
                ) : null}
              </div>
              <footer className="settings-savebar">
                {conflict ? (
                  <div className="settings-conflict" role="alert">
                    <span><Warning size={15} />The configuration was changed by another client. Saving now will overwrite those changes.</span>
                    <button onClick={load} disabled={saving}>Reload</button>
                    <button onClick={() => save(true)} disabled={saving}>Overwrite</button>
                  </div>
                ) : null}
                {saveError ? <p className="settings-save-error" role="alert">{saveError}</p> : null}
                <div className="settings-savebar-row">
                  <div className="settings-savebar-status">
                    {dirty ? <span className="settings-dirty">Unsaved changes</span> : null}
                    {savedOk && !dirty ? (
                      <span className="settings-save-ok" role="status"><CheckCircle size={15} />Saved</span>
                    ) : null}
                  </div>
                  <div className="settings-savebar-actions">
                    <button className="settings-button" onClick={requestClose} disabled={saving}>Cancel</button>
                    <button
                      className="settings-button settings-button-primary"
                      onClick={() => save(false)}
                      disabled={!canSaveDraft(dirty, errors, saving)}
                    >
                      {saving ? "Saving…" : "Save all"}
                    </button>
                  </div>
                </div>
              </footer>
            </div>
          </div>
        ) : null}
      </section>
    </div>
  );
}
