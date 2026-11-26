const view = document.querySelector("#session-view");
const showOffline = document.querySelector("#show-offline");
const counts = document.querySelector("#node-counts");
const table = document.querySelector("#session-table-scroll");
const rows = document.querySelector("#session-rows");
const feedback = document.querySelector("#session-feedback");

const statuses = new Map([
  ["waiting", {label: "Waiting", rank: 0}],
  ["busy", {label: "Busy", rank: 1}],
  ["idle", {label: "Idle", rank: 2}],
  ["unknown", {label: "Unknown", rank: 3}],
  ["offline", {label: "Offline", rank: 4}],
]);
const providers = new Map([["claude", "Claude"], ["codex", "Codex"], ["agy", "Antigravity"]]);
const names = new Intl.Collator(undefined, {numeric: true});
const dateTime = new Intl.DateTimeFormat(undefined, {dateStyle: "medium", timeStyle: "short"});

let sessions = [];
let signature = null;
let request;
let poller;
let refreshFailed = false;

function label(tag, text) {
  const node = document.createElement(tag);
  node.textContent = text;
  return node;
}

function announce(message, error = false) {
  if (feedback.textContent !== message) feedback.textContent = message;
  feedback.dataset.error = error;
}

function currentStatus(session) {
  if (!session.online) return "offline";
  return statuses.has(session.status) ? session.status : "unknown";
}

function byPriority(left, right) {
  return statuses.get(currentStatus(left)).rank - statuses.get(currentStatus(right)).rank ||
    names.compare(left.node, right.node) ||
    names.compare(left.provider, right.provider) ||
    names.compare(left.name || left.id, right.name || right.id) ||
    names.compare(left.id, right.id);
}

function updateElapsedTimes() {
  const now = Date.now() / 1000;
  for (const item of rows.querySelectorAll("time[data-since]")) {
    const seconds = Math.max(0, Math.floor(now - Number(item.dataset.since)));
    const days = Math.floor(seconds / 86400);
    const hours = Math.floor(seconds / 3600) % 24;
    const minutes = Math.floor(seconds / 60) % 60;
    if (days) item.textContent = days + "d " + hours + "h";
    else if (hours) item.textContent = hours + "h " + minutes + "m";
    else if (minutes) item.textContent = minutes + "m " + (seconds % 60) + "s";
    else item.textContent = seconds + "s";
  }
}

function renderCounts() {
  const nodes = new Map();
  for (const session of sessions) {
    if (!nodes.has(session.node)) nodes.set(session.node, {total: 0, online: 0});
    const count = nodes.get(session.node);
    count.total += 1;
    count.online += Number(session.online);
  }
  const items = [];
  for (const [node, count] of [...nodes].sort(([left], [right]) => names.compare(left, right))) {
    const item = document.createElement("li");
    item.append(label("strong", node), label("span", count.online + "/" + count.total + " online"));
    items.push(item);
  }
  counts.replaceChildren(...items);
}

function renderRow(session) {
  const current = currentStatus(session);
  const row = document.createElement("tr");
  row.dataset.status = current;

  const name = label("th", session.name || session.id);
  name.scope = "row";
  name.title = session.id;
  const status = label("span", statuses.get(current).label);
  status.className = "status";
  status.dataset.status = current;
  const state = document.createElement("td");
  state.append(status);

  const elapsed = label("td", "—");
  if (Number.isFinite(session.status_since)) {
    const start = new Date(session.status_since * 1000);
    const time = document.createElement("time");
    time.dataset.since = session.status_since;
    time.dateTime = start.toISOString();
    time.title = dateTime.format(start);
    elapsed.replaceChildren(time);
  }

  const tmux = label("td", session.tmux ? session.node + " " + session.tmux : "—");
  const cwd = label("td", session.cwd || "—");
  tmux.className = "session-path";
  cwd.className = "session-path";
  row.append(label("td", session.node), label("td", providers.get(session.provider) || session.provider),
    name, state, label("td", current === "waiting" && session.waiting_for ? session.waiting_for : "—"), elapsed, tmux, cwd);
  return row;
}

function renderTable() {
  const visible = sessions.filter(session => session.online || showOffline.checked).sort(byPriority);
  const fragment = document.createDocumentFragment();
  for (const session of visible) fragment.append(renderRow(session));
  rows.replaceChildren(fragment);
  table.hidden = visible.length === 0;
  updateElapsedTimes();
}

function renderFeedback() {
  if (refreshFailed) {
    announce(signature === null ? "Could not load sessions. Use Refresh to try again." :
      "Could not refresh sessions. Showing the last results.", true);
  } else if (signature === null) {
    announce("Loading sessions…");
  } else if (!sessions.length) {
    announce("No sessions have been published.");
  } else {
    announce(table.hidden ? "No online sessions. Select Show offline sessions to see the last published sessions." : "");
  }
}

export async function refreshSessions() {
  if (view.hidden || request) return;
  const pending = new AbortController();
  request = pending;
  updateElapsedTimes();

  try {
    const signal = AbortSignal.any([pending.signal, AbortSignal.timeout(10000)]);
    const response = await fetch("/sessions", {signal, cache: "no-store"});
    if (!response.ok) throw new Error(String(response.status));
    const data = await response.json();
    if (pending.signal.aborted) return;
    if (!Array.isArray(data.sessions)) throw new Error("Invalid sessions response");
    const nextSignature = JSON.stringify(data.sessions);
    if (signature !== nextSignature) {
      sessions = data.sessions;
      renderCounts();
      renderTable();
      signature = nextSignature;
    }
    refreshFailed = false;
    renderFeedback();
  } catch {
    if (!pending.signal.aborted) {
      refreshFailed = true;
      renderFeedback();
    }
  } finally {
    if (request === pending) request = null;
  }
}

export function showSessions(visible) {
  if (view.hidden === !visible) return;
  view.hidden = !visible;
  if (visible) {
    refreshSessions();
    poller = window.setInterval(refreshSessions, 5000);
  } else {
    window.clearInterval(poller);
    request?.abort();
    request = null;
  }
}

showOffline.addEventListener("change", () => {
  renderTable();
  renderFeedback();
});
