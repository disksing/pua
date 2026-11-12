import { Call } from "/wails/runtime.js";

const call = (method, ...args) => Call.ByName(`main.DesktopService.${method}`, ...args);
const $ = (id) => document.getElementById(id);
let latest;

function showNotice(message, error = false) {
  const node = $("notice");
  node.hidden = !message;
  node.textContent = message || "";
  node.style.background = error ? "#fef2f2" : "#eff6ff";
  node.style.color = error ? "#991b1b" : "#1e40af";
}

function component(prefix, value) {
  $(`${prefix}-version`).textContent = value.version ? `v${value.version}` : "Version unavailable";
  const state = $(`${prefix}-state`);
  state.textContent = value.state;
  state.className = `badge ${value.state}`;
  $(`${prefix}-endpoint`).textContent = value.endpoint || "—";
}

function render(status) {
  latest = status;
  $("app-version").textContent = `PUA.app ${status.appVersion} · manager v${status.desktopManagerProtocol}`;
  component("pua", status.pua);
  component("agenthub", status.agentHub);
  $("pua-process").textContent = status.pua.pid ? `PID ${status.pua.pid}${status.pua.managed ? " · managed" : " · external"}` : "—";
  $("active-turns").textContent = String(status.activeTurns || 0);
  if (!["host", "port"].includes(document.activeElement?.id)) {
    $("host").value = status.config.host;
    $("port").value = status.config.port;
  }
  if (document.activeElement?.id !== "auto-check") $("auto-check").checked = status.config.autoCheck;
  $("exposure-warning").hidden = !status.exposed;
  $("exposure-warning").textContent = status.exposureWarning || "";
  document.querySelectorAll("[data-action='stop'],[data-action='restart']").forEach((button) => button.disabled = !status.pua.managed && !status.agentHub.managed);
  document.querySelector("[data-action='start']").disabled = status.pua.state !== "stopped" || status.agentHub?.state === "external";
  renderUpdates(status.updates);
}

function renderUpdates(check) {
  if (!check || !check.state) return;
  if (check.error) {
    $("update-summary").textContent = check.error;
    $("install-updates").hidden = true;
    return;
  }
  const items = [];
  if (check.plan?.pua) items.push(`PUA Server ${check.plan.pua.release.version}`);
  if (check.plan?.agentHub) items.push(`AgentHub ${check.plan.agentHub.release.version}`);
  if (check.plan?.appUpdateRequired) items.push(`PUA.app required (manager v${check.plan.requiredManager})`);
  $("update-summary").textContent = items.length ? `Available: ${items.join(" · ")}` : "PUA Server and AgentHub are current.";
  $("install-updates").hidden = !check.plan || (!check.plan.pua && !check.plan.agentHub) || check.plan.appUpdateRequired;
}

async function refresh() {
  try { render(await call("Status")); } catch (error) { showNotice(String(error), true); }
}

async function action(name, args = []) {
  document.querySelectorAll("button").forEach((button) => button.disabled = true);
  showNotice(`${name}…`);
  try {
    await call(name, ...args);
    showNotice(`${name} completed.`);
  } catch (error) {
    showNotice(String(error), true);
  } finally {
    document.querySelectorAll("button").forEach((button) => button.disabled = false);
    await refresh();
  }
}

document.querySelectorAll("[data-action]").forEach((button) => button.addEventListener("click", () => {
  const name = button.dataset.action[0].toUpperCase() + button.dataset.action.slice(1);
  if (["Stop", "Restart"].includes(name) && latest?.activeTurns > 0 &&
      !window.confirm(`AgentHub has ${latest.activeTurns} active turn(s). ${name} will interrupt that work. Continue?`)) return;
  action(name);
}));
$("config-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const restart = await call("SaveConfig", {schemaVersion: 1, host: $("host").value, port: Number($("port").value), autoCheck: $("auto-check").checked});
    showNotice(restart ? "Settings saved. Restart services to use the new address." : "Settings saved.");
  } catch (error) { showNotice(String(error), true); }
  await refresh();
});
$("check-updates").addEventListener("click", () => action("CheckUpdates"));
$("install-updates").addEventListener("click", () => action("InstallUpdates", [[]]));
$("open-fda").addEventListener("click", () => action("OpenFullDiskAccessSettings"));
refresh();
setInterval(refresh, 5000);
