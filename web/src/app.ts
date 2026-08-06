// The reference web client: a non-coder opens their site key, sees their sites,
// describes what they want in plain words, watches Creo build it live, and puts
// it online — all through the public API (Api). Three screens (key / home /
// workspace) share one event stream; the stream is the single source of truth
// for the transcript, so messages survive reload and second-device resume.
import {
  Api,
  ApiError,
  isUnauthorized,
  type Event,
  type LoginFlow,
  type Principal,
  type Project,
  type Version,
} from "./api";

const api = new Api("", localStorage.getItem("creo_token") ?? "");

type Screen = "key" | "home" | "workspace";

interface State {
  screen: Screen;
  projectId: string;
  sessionId: string;
  siteTitle: string;
  lastSeq: number;
  building: boolean;
  hasVersion: boolean;
  publishedUrl: string;
  steps: string[]; // distinct build-progress phrases for the current run
  who: Principal | null;
}
const state: State = {
  screen: "key",
  projectId: "",
  sessionId: "",
  siteTitle: "Your site",
  lastSeq: 0,
  building: false,
  hasVersion: false,
  publishedUrl: "",
  steps: [],
  who: null,
};
let unsub: (() => void) | null = null;
// The family-mode banner's dismissal deliberately lives here and nowhere else:
// page memory, not sessionStorage — so it returns on every reload and in every
// new tab. An honest warning you can permanently silence isn't one.
let bannerDismissed = false;

const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T | null;

// A warm, deterministic hero colour per site, so a site looks like itself
// across the home card, the publish chip, and the live badge.
function heroColor(seed: string): string {
  const palette = ["#3E6B4F", "#B5651D", "#7A5A1B", "#8C5A6B", "#4F5B6B", "#6B5B3E"];
  let h = 0;
  for (const c of seed) h = (h * 31 + c.charCodeAt(0)) >>> 0;
  return palette[h % palette.length];
}

// ---------------------------------------------------------------- screens
function showScreen(name: Screen) {
  state.screen = name;
  for (const s of ["key", "home", "workspace"] as Screen[]) {
    $(`screen-${s}`)?.classList.toggle("hidden", s !== name);
  }
}

// ---------------------------------------------------------------- who am I
// The picker is the whole login: names, no secrets. Whether that is honest is
// a property of the deployment, and the platform says so via `assurance` —
// which is what the banner keys off, never the driver's name.
let currentFlow: LoginFlow | null = null;

async function showAccountPicker(): Promise<void> {
  const list = $("account-list");
  showScreen("key");
  if (!list) return;
  list.innerHTML = "";
  try {
    currentFlow = await api.beginLogin();
  } catch {
    currentFlow = null;
  }
  const choices = currentFlow?.choices ?? [];
  if (choices.length === 0) {
    list.innerHTML =
      '<div id="account-empty">No one has been set up on this server yet. ' +
      "Whoever runs it can add you with <code>creo account new</code>.</div>";
    return;
  }
  for (const c of choices) {
    const btn = document.createElement("button");
    btn.className = "account";
    btn.innerHTML =
      `<span class="face" style="background:${c.color || heroColor(c.id)}">` +
      `${escapeHtml((c.name[0] || "?").toUpperCase())}</span>` +
      `<span>${escapeHtml(c.name)}</span>`;
    btn.addEventListener("click", () => void pickAccount(c.id));
    list.appendChild(btn);
  }
}

async function pickAccount(accountId: string) {
  if (!currentFlow) return;
  try {
    state.who = await api.completeLogin(currentFlow.flowId, accountId);
    bannerDismissed = false; // a new sign-in re-earns the warning
    $("key-error")?.classList.add("hidden");
    if (!(await loadHome())) await showAccountPicker();
  } catch (err) {
    const el = $("key-error");
    if (el) {
      // 400 = the flow expired (a stale picker); anything else = can't sign in.
      el.textContent =
        err instanceof ApiError && err.status === 400
          ? "That took a while — please pick your name again."
          : "That account can't sign in right now.";
      el.classList.remove("hidden");
    }
    await showAccountPicker();
  }
}

// renderWho paints the signed-in identity and the family-mode banner. Called
// on every entry to the home screen so a second device shows the truth too.
function renderWho() {
  const who = $("home-who");
  if (who) {
    who.textContent = state.who?.name ? `Signed in as ${state.who.name}` : "";
    who.classList.toggle("hidden", !state.who?.name);
  }
  $("home-signout")?.classList.toggle("hidden", !state.who && !api.hasToken());

  const banner = $("family-banner");
  if (!banner) return;
  const attributed = state.who?.assurance === "attributed";
  const text = $("family-banner-text");
  if (text) {
    text.textContent = "Family mode — anyone who can reach this server can use these accounts.";
  }
  banner.classList.toggle("hidden", !attributed || bannerDismissed);
}

function dismissBanner() {
  bannerDismissed = true;
  $("family-banner")?.classList.add("hidden");
}

// ---------------------------------------------------------------- chat log
function addMessage(kind: "you" | "creo" | "note", text: string) {
  const log = $("log");
  if (!log) return;
  const el = document.createElement("div");
  el.className = `msg ${kind}`;
  el.textContent = text;
  // Keep the live build card pinned to the bottom of the transcript.
  const card = document.getElementById("build-card");
  if (card) log.insertBefore(el, card);
  else log.appendChild(el);
  log.scrollTop = log.scrollHeight;
}

// Fire-and-forget entry points (bootstrap, screen switches) can reject after the
// call site has returned; without this their failure would vanish as an unhandled
// rejection and the user would just see a frozen screen. `void` is used instead
// wherever the callee provably handles its own errors.
function reportFailure(err: unknown) {
  console.error("Creo:", err);
  addMessage("note", "Something went wrong. Please refresh the page and try again.");
}

// The build card is a single, self-replacing checklist — live progress, never
// part of the saved transcript. Distinct phrases from tool.result become steps;
// earlier steps show a check, the newest is active.
function renderBuild() {
  const log = $("log");
  if (!log) return;
  let card = document.getElementById("build-card");
  if (!card) {
    card = document.createElement("div");
    card.id = "build-card";
    card.innerHTML =
      '<div class="head"><span class="dots"><span></span><span></span><span></span></span>' +
      '<span>Building your site</span></div><div id="build-steps"></div>';
    log.appendChild(card);
  } else {
    log.appendChild(card); // move to bottom
  }
  const steps = card.querySelector("#build-steps")!;
  steps.innerHTML = "";
  const items = state.steps.length ? state.steps : ["Getting started"];
  items.forEach((label, i) => {
    const active = i === items.length - 1;
    const row = document.createElement("div");
    row.className = "step " + (active ? "active" : "done");
    row.innerHTML = `<span class="mark">${active ? "●" : "✓"}</span><span>${label}</span>`;
    steps.appendChild(row);
  });
  log.scrollTop = log.scrollHeight;
}

function clearBuild() {
  document.getElementById("build-card")?.remove();
  state.steps = [];
}

// ---------------------------------------------------------------- preview
function updatePreviewState() {
  const ws = $("screen-workspace");
  if (!ws) return;
  const mode = state.hasVersion ? "ready" : state.building ? "building" : "empty";
  ws.setAttribute("data-preview", mode);
}

function setBuilding(b: boolean) {
  state.building = b;
  $("screen-workspace")?.classList.toggle("building", b);
  const send = $<HTMLButtonElement>("send");
  if (send) send.disabled = b;
  const pub = $<HTMLButtonElement>("ws-publish");
  if (pub) pub.disabled = b || !state.hasVersion;
  const st = $("ws-status-text");
  if (st) st.textContent = b ? "Creo is working…" : "Ready";
  if (!b) clearBuild();
  updatePreviewState();
}

async function refreshPreview() {
  if (!state.projectId) return;
  try {
    const { url } = await api.preview(state.projectId);
    const frame = $<HTMLIFrameElement>("preview");
    if (frame) frame.src = url + "?t=" + Date.now();
    state.hasVersion = true;
    const pub = $<HTMLButtonElement>("ws-publish");
    if (pub) pub.disabled = state.building;
    updatePreviewState();
  } catch {
    /* no version yet */
  }
}

// ---------------------------------------------------------------- events
function handleEvent(e: Event) {
  if (e.seq <= state.lastSeq) return;
  state.lastSeq = e.seq;
  switch (e.type) {
    case "user.message":
      // Single source of truth: the user's own words render here — live AND on
      // reload/second-device resume. No optimistic echo in send().
      if (e.userText) {
        addMessage("you", e.userText);
        $("chips")?.classList.add("hidden");
      }
      break;
    case "run.started":
    case "run.resumed":
      state.steps = [];
      setBuilding(true);
      renderBuild();
      break;
    case "tool.result":
      // Plain-language build progress, authored server-side (fix-01).
      if (e.userText) {
        if (state.steps[state.steps.length - 1] !== e.userText) state.steps.push(e.userText);
        renderBuild();
        const t = $("preview-building-text");
        if (t) t.textContent = e.userText + "…";
      }
      break;
    case "assistant.message":
      // A working turn with commentary — a real transcript bubble.
      if (e.userText) addMessage("creo", e.userText);
      break;
    case "run.completed":
      // The sole completion message (fix-02 blanks the final assistant text).
      if (e.userText) addMessage("creo", e.userText);
      break;
    case "run.failed":
      if (e.userText) addMessage("note", e.userText);
      break;
    case "error.translated":
      // Gentle, user-facing problems surface in the banner, not as a red error.
      if (e.userText) showBanner(e.userText);
      break;
    case "preview.ready":
    case "artifact.version.created":
      void refreshPreview();
      break;
    case "publish.completed":
    case "publish.rolled_back":
      if (e.userText) addMessage("note", e.userText);
      break;
  }
  if (e.type === "run.completed" || e.type === "run.failed") {
    setBuilding(false);
    void refreshPreview();
  }
}

function showBanner(text: string) {
  const b = $("limit-banner");
  if (!b) return;
  b.textContent = text;
  b.classList.remove("hidden");
}

// ---------------------------------------------------------------- project lifecycle
function subscribe() {
  unsub?.();
  unsub = api.streamEvents(state.sessionId, state.lastSeq, handleEvent);
}

function resetWorkspace(title: string) {
  state.lastSeq = 0;
  state.building = false;
  state.hasVersion = false;
  state.steps = [];
  state.siteTitle = title;
  const t = $("ws-title");
  if (t) t.textContent = title;
  const st = $("ws-status-text");
  if (st) st.textContent = "Ready";
  const log = $("log");
  if (log) {
    log.innerHTML =
      "<div class=\"msg creo\">Tell me about the website you'd like — what it's for, " +
      "who it's for, and any details you have.</div>";
  }
  $("chips")?.classList.remove("hidden");
  $("limit-banner")?.classList.add("hidden");
  $("live-link")?.classList.add("hidden");
  const pub = $<HTMLButtonElement>("ws-publish");
  if (pub) pub.disabled = true;
  updatePreviewState();
}

async function openProject(p: Project) {
  state.projectId = p.id;
  state.sessionId = p.sessionId;
  localStorage.setItem("creo_project", p.id);
  localStorage.setItem("creo_session", p.sessionId);
  localStorage.setItem("creo_title", p.name);
  resetWorkspace(p.name || "Your site");
  showScreen("workspace");
  try {
    const events = await api.fetchEvents(p.sessionId, 0);
    for (const e of events) handleEvent(e);
  } catch {
    /* fresh session, nothing to replay */
  }
  subscribe();
  void refreshPreview();
}

async function newProject() {
  const p = await api.createProject("Your site");
  await openProject(p);
}

async function loadHome(): Promise<boolean> {
  let projects: Project[];
  try {
    projects = await api.listProjects();
  } catch (err) {
    if (isUnauthorized(err)) return false; // not signed in — the caller shows sign-in
    projects = [];
  }
  if (!state.who) {
    try {
      state.who = await api.me();
    } catch {
      /* token mode on a server without identity, or a stale cookie */
    }
  }
  renderHome(projects);
  showScreen("home");
  renderWho();
  return true;
}

function renderHome(projects: Project[]) {
  const empty = $("home-empty");
  const grid = $("site-grid");
  if (!empty || !grid) return;
  if (projects.length === 0) {
    empty.classList.remove("hidden");
    grid.classList.add("hidden");
    return;
  }
  empty.classList.add("hidden");
  grid.classList.remove("hidden");
  grid.innerHTML = "";
  for (const p of projects) {
    const card = document.createElement("div");
    card.className = "site-card";
    const color = heroColor(p.id);
    card.innerHTML =
      `<div class="hero" style="background:${color}">${escapeHtml(p.name || "Your site")}</div>` +
      `<div class="meta"><div class="row"><b>${escapeHtml(p.name || "Your site")}</b></div>` +
      `<div class="sub">Open to keep building →</div></div>`;
    card.addEventListener("click", () => openProject(p));
    grid.appendChild(card);
  }
  const add = document.createElement("div");
  add.className = "site-card new";
  add.textContent = "＋  Start a new site";
  add.addEventListener("click", () => newProject());
  grid.appendChild(add);
}

function escapeHtml(s: string): string {
  return s.replace(
    /[&<>"]/g,
    (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]!,
  );
}

// ---------------------------------------------------------------- composer
async function send() {
  const input = $<HTMLTextAreaElement>("input");
  if (!input) return;
  const text = input.value.trim();
  if (!text || state.building) return;
  input.value = "";
  setBuilding(true);
  try {
    if (!state.projectId) {
      const p = await api.createProject("Your site");
      state.projectId = p.id;
      state.sessionId = p.sessionId;
      localStorage.setItem("creo_project", p.id);
      localStorage.setItem("creo_session", p.sessionId);
      subscribe();
    }
    await api.sendMessage(state.sessionId, text, crypto.randomUUID());
  } catch (err) {
    addMessage("note", String(err));
    setBuilding(false);
  }
}

// ---------------------------------------------------------------- publish
function openPublishModal() {
  if (!state.hasVersion) return;
  const slug =
    state.siteTitle
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-|-$/g, "") || "your-site";
  const hero = $("publish-hero");
  if (hero) {
    hero.textContent = state.siteTitle;
    hero.style.background = heroColor(state.projectId);
  }
  const url = $("publish-url");
  if (url) url.textContent = `${slug}.creo.site`;
  const already = state.publishedUrl;
  const title = $("publish-title");
  const body = $("publish-body");
  const go = $("publish-go");
  if (already) {
    if (title) title.textContent = "Publish your latest changes?";
    if (body)
      body.textContent =
        "Your site is already online — this will update it with everything you've changed since.";
    if (go) go.textContent = "Yes, update my site";
  } else {
    if (title) title.textContent = "Put your site online?";
    if (body)
      body.textContent =
        "Anyone with the link will be able to visit it. You can keep editing afterwards — nothing changes online until you publish again.";
    if (go) go.textContent = "Yes, put it online";
  }
  $("publish-modal")?.classList.remove("hidden");
}

async function doPublish() {
  $("publish-modal")?.classList.add("hidden");
  try {
    const { url } = await api.publish(state.projectId);
    state.publishedUrl = url;
    localStorage.setItem("creo_pub_" + state.projectId, url);
    const link = $<HTMLAnchorElement>("live-link");
    if (link) {
      link.href = url;
      link.classList.remove("hidden");
    }
    celebrate(url);
  } catch (err) {
    addMessage("note", String(err));
  }
}

function celebrate(url: string) {
  const title = $("celebrate-title");
  if (title) title.textContent = `${state.siteTitle} is live!`;
  const u = $("celebrate-url");
  const display = url.replace(/^https?:\/\//, "");
  if (u) u.textContent = display;
  spawnConfetti();
  $("celebration")?.classList.remove("hidden");
}

function spawnConfetti() {
  const host = $("celebration");
  if (!host) return;
  host.querySelectorAll(".confetti").forEach((c) => c.remove());
  const colors = ["#E9A23B", "#3E6B4F", "#B5651D", "#8C5A6B"];
  for (let i = 0; i < 24; i++) {
    const c = document.createElement("span");
    c.className = "confetti";
    c.style.left = Math.floor((i / 24) * 100) + "%";
    c.style.background = colors[i % colors.length];
    c.style.animationDuration = 2.6 + (i % 5) * 0.4 + "s";
    c.style.animationDelay = (i % 7) * 0.15 + "s";
    host.appendChild(c);
  }
}

// ---------------------------------------------------------------- history
async function openHistory() {
  const list = $("history-list");
  const drawer = $("history-drawer");
  if (!list || !drawer) return;
  drawer.classList.remove("hidden");
  list.innerHTML = '<div class="empty">Loading your history…</div>';
  let versions: Version[] = [];
  try {
    versions = await api.versions(state.projectId);
  } catch {
    /* none yet */
  }
  if (versions.length === 0) {
    list.innerHTML =
      '<div class="empty">Your first version will appear here once building finishes.</div>';
    return;
  }
  list.innerHTML = "";
  versions.forEach((v, i) => {
    const isCurrent = i === 0; // newest-first
    const row = document.createElement("div");
    row.className = "ver";
    const badge = isCurrent ? '<span class="pill current">Current</span>' : "";
    row.innerHTML =
      `<div class="row"><span class="swatch" style="background:${heroColor(v.id)}"></span>` +
      `<span class="info"><span class="t">${isCurrent ? "This is your site now" : "An earlier version"}</span>` +
      `<span class="s">Saved ${friendlyTime(v.createdAt)}</span></span>${badge}</div>`;
    list.appendChild(row);
  });
  const foot = document.createElement("div");
  foot.className = "foot";
  foot.textContent = "Going back to an earlier version is on the way.";
  list.appendChild(foot);
}

function friendlyTime(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "recently";
  return d.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });
}

// ---------------------------------------------------------------- bootstrap
async function bootstrap() {
  // Prefer resuming the last-opened project (keeps the workspace on reload).
  const pid = localStorage.getItem("creo_project");
  const sid = localStorage.getItem("creo_session");
  if (pid && sid) {
    const name = localStorage.getItem("creo_title") || "Your site";
    state.publishedUrl = localStorage.getItem("creo_pub_" + pid) || "";
    try {
      await openProject({ id: pid, sessionId: sid, name });
      return;
    } catch {
      localStorage.removeItem("creo_project");
      localStorage.removeItem("creo_session");
    }
  }
  // Otherwise land on the sites list — falling back to the picker if the
  // server wants a signed-in human.
  if (!(await loadHome())) await showAccountPicker();
}

export function init() {
  // Sign-in screen: the picker is the default; a token field is the operator
  // escape hatch (and what the CLI/tests use).
  $("key-operator")?.addEventListener("click", () => {
    $("key-token")?.classList.remove("hidden");
    $("key-operator")?.classList.add("hidden");
    $<HTMLInputElement>("key-input")?.focus();
  });
  $("key-go")?.addEventListener("click", async () => {
    const input = $<HTMLInputElement>("key-input");
    const key = input?.value.trim() ?? "";
    api.setToken(key);
    localStorage.setItem("creo_token", key);
    if (await loadHome()) $("key-error")?.classList.add("hidden");
    else {
      const err = $("key-error");
      if (err) {
        err.textContent = "That token didn't open anything. Check it and try again.";
        err.classList.remove("hidden");
      }
    }
  });

  // Family-mode banner: hide for this page view only.
  $("family-banner-close")?.addEventListener("click", dismissBanner);

  // Home screen
  $("home-empty-btn")?.addEventListener("click", () => newProject());
  $("home-empty-cta")?.addEventListener("click", () => newProject());
  $("home-signout")?.addEventListener("click", async () => {
    localStorage.removeItem("creo_token");
    localStorage.removeItem("creo_project");
    localStorage.removeItem("creo_session");
    api.setToken("");
    state.who = null;
    try {
      await api.logout();
    } catch {
      /* already signed out */
    }
    await showAccountPicker();
  });

  // Workspace
  $("send")?.addEventListener("click", send);
  $("input")?.addEventListener("keydown", (ev) => {
    const k = ev as KeyboardEvent;
    if (k.key === "Enter" && !k.shiftKey) {
      ev.preventDefault();
      void send();
    }
  });
  for (const chip of Array.from(document.querySelectorAll<HTMLElement>("#chips .chip"))) {
    chip.addEventListener("click", () => {
      const input = $<HTMLTextAreaElement>("input");
      if (input) {
        input.value = chip.textContent || "";
        void send();
      }
    });
  }
  $("ws-back")?.addEventListener("click", () => {
    unsub?.();
    unsub = null;
    localStorage.removeItem("creo_project");
    localStorage.removeItem("creo_session");
    state.projectId = "";
    state.sessionId = "";
    loadHome().catch(reportFailure);
  });
  $("ws-publish")?.addEventListener("click", openPublishModal);
  $("ws-history")?.addEventListener("click", openHistory);

  // History drawer
  $("history-close")?.addEventListener("click", () => $("history-drawer")?.classList.add("hidden"));

  // Publish modal + celebration
  $("publish-go")?.addEventListener("click", doPublish);
  $("publish-cancel")?.addEventListener("click", () => $("publish-modal")?.classList.add("hidden"));
  $("celebrate-close")?.addEventListener("click", () => $("celebration")?.classList.add("hidden"));
  $("celebrate-copy")?.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(state.publishedUrl);
      const btn = $("celebrate-copy");
      if (btn) btn.textContent = "Copied ✓";
    } catch {
      /* clipboard blocked */
    }
  });

  bootstrap().catch(reportFailure);
}

// Wire up on load (skipped under test, which imports functions directly).
if (typeof document !== "undefined" && document.getElementById("send")) {
  init();
}

// exported for teardown / tests
export { state, unsub, handleEvent, renderWho, showAccountPicker, dismissBanner };
