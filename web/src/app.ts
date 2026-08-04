// The reference web client: a non-coder describes a site, watches it build,
// previews it live, and publishes — all through the public API (Api).
import { Api, type Event } from "./api";

const api = new Api("", localStorage.getItem("creo_token") ?? "");

interface State {
  projectId: string;
  sessionId: string;
  lastSeq: number;
  building: boolean;
  publishedUrl: string;
}
const state: State = { projectId: "", sessionId: "", lastSeq: 0, building: false, publishedUrl: "" };
let unsub: (() => void) | null = null;

const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;

function addMessage(who: "you" | "creo" | "system", text: string) {
  const log = $("log");
  const el = document.createElement("div");
  el.className = `msg ${who}`;
  el.textContent = text;
  log.appendChild(el);
  log.scrollTop = log.scrollHeight;
}

function setBuilding(b: boolean) {
  state.building = b;
  $<HTMLButtonElement>("send").disabled = b;
  $<HTMLButtonElement>("publish").disabled = b || !state.projectId;
  $("status").textContent = b ? "Creo is working…" : "Ready";
}

async function refreshPreview() {
  if (!state.projectId) return;
  try {
    const { url } = await api.preview(state.projectId);
    // cache-bust so the iframe reloads on each new version
    $<HTMLIFrameElement>("preview").src = url + "?t=" + Date.now();
  } catch {
    /* no version yet */
  }
}

function handleEvent(e: Event) {
  if (e.seq <= state.lastSeq) return;
  state.lastSeq = e.seq;
  switch (e.type) {
    case "assistant.message":
    case "run.completed":
      if (e.userText) addMessage("creo", e.userText);
      break;
    case "run.failed":
    case "error.translated":
      if (e.userText) addMessage("system", e.userText);
      break;
    case "preview.ready":
    case "artifact.version.created":
      refreshPreview();
      break;
    case "publish.completed":
    case "publish.rolled_back":
      if (e.userText) addMessage("system", e.userText);
      break;
  }
  if (e.type === "run.completed" || e.type === "run.failed") {
    setBuilding(false);
    refreshPreview();
  }
}

async function ensureProject() {
  if (state.projectId) return;
  const p = await api.createProject("My site");
  state.projectId = p.id;
  state.sessionId = p.sessionId;
  localStorage.setItem("creo_project", p.id);
  localStorage.setItem("creo_session", p.sessionId);
  unsub = api.streamEvents(state.sessionId, state.lastSeq, handleEvent);
}

async function send() {
  const input = $<HTMLTextAreaElement>("input");
  const text = input.value.trim();
  if (!text || state.building) return;
  input.value = "";
  addMessage("you", text);
  setBuilding(true);
  try {
    await ensureProject();
    await api.sendMessage(state.sessionId, text, crypto.randomUUID());
  } catch (err) {
    addMessage("system", String(err));
    setBuilding(false);
  }
}

async function publish() {
  if (!state.projectId) return;
  try {
    const { url } = await api.publish(state.projectId);
    state.publishedUrl = url;
    addMessage("system", `Published: ${url}`);
    $<HTMLAnchorElement>("live").href = url;
    $("live").textContent = "View live site";
  } catch (err) {
    addMessage("system", String(err));
  }
}

async function hydrate() {
  const pid = localStorage.getItem("creo_project");
  const sid = localStorage.getItem("creo_session");
  if (!pid || !sid) return;
  state.projectId = pid;
  state.sessionId = sid;
  try {
    const events = await api.fetchEvents(sid, 0);
    for (const e of events) handleEvent(e);
    unsub = api.streamEvents(sid, state.lastSeq, handleEvent);
    $<HTMLButtonElement>("publish").disabled = false;
    refreshPreview();
  } catch {
    // stale project (e.g. fresh data dir) — start over on next send
    localStorage.removeItem("creo_project");
    localStorage.removeItem("creo_session");
    state.projectId = "";
    state.sessionId = "";
  }
}

export function init() {
  $("send").addEventListener("click", send);
  $("publish").addEventListener("click", publish);
  $("input").addEventListener("keydown", (ev) => {
    if ((ev as KeyboardEvent).key === "Enter" && !(ev as KeyboardEvent).shiftKey) {
      ev.preventDefault();
      send();
    }
  });
  const tokenInput = $<HTMLInputElement>("token");
  tokenInput.value = localStorage.getItem("creo_token") ?? "";
  tokenInput.addEventListener("change", () => {
    localStorage.setItem("creo_token", tokenInput.value.trim());
    api.setToken(tokenInput.value.trim());
  });
  hydrate();
}

// Wire up on load (skipped under test, which imports functions directly).
if (typeof document !== "undefined" && document.getElementById("send")) {
  init();
}

// exported for potential teardown / tests
export { state, unsub };
