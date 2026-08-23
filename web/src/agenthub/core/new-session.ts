// @ts-nocheck -- pure form model retained with its behavior tests.
// Pure form model for the New Session dialog.

export const TITLE_MAX_LENGTH = 120;

export function normalizeTitle(value) {
  return String(value ?? "").trim();
}

export function normalizeCwd(value) {
  return String(value ?? "").trim();
}

// Returns an error message, or "" when the value is acceptable. The daemon
// performs the authoritative checks (exists, is a directory, permissions);
// this only guards obviously empty input before submission.
export function validateCwd(value) {
  if (!normalizeCwd(value)) return "Working directory is required.";
  return "";
}

export function validateTitle(value) {
  if (normalizeTitle(value).length > TITLE_MAX_LENGTH) {
    return `Title must be ${TITLE_MAX_LENGTH} characters or fewer.`;
  }
  return "";
}

export function validateAgent(agentName, agents) {
  if (!agentName) return "Select an agent.";
  if (Array.isArray(agents) && agents.length && !agents.some((agent) => agent.name === agentName)) {
    return "The selected agent is no longer available.";
  }
  return "";
}

// Returns { title?, cwd, agentName } ready for POST /v1/sessions, or null
// when any field is invalid. An empty title is omitted so the daemon applies
// its default title.
export function buildCreatePayload({ title, cwd, agentName, agents }) {
  const errors = {
    title: validateTitle(title),
    cwd: validateCwd(cwd),
    agent: validateAgent(agentName, agents),
  };
  if (errors.title || errors.cwd || errors.agent) return { errors, payload: null };
  const payload = { cwd: normalizeCwd(cwd), agentName };
  const clean = normalizeTitle(title);
  if (clean) payload.title = clean;
  return { errors, payload };
}
