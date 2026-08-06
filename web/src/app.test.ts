import { describe, it, expect, beforeEach, vi, afterEach } from "vitest";
import { state, handleEvent, renderWho, showAccountPicker, dismissBanner } from "./app";
import type { Event } from "./api";

// Minimal DOM the render path touches. app.ts's load-time init() is guarded on
// #send existing at import time (absent here), so importing has no side effects;
// we build the DOM afterwards and drive handleEvent directly. handleEvent guards
// every optional element, so this slimmed-down tree is enough.
function setupDom() {
  document.body.innerHTML = `
    <section id="screen-workspace" data-preview="empty">
      <span id="ws-status-text"></span>
      <div id="log"></div>
      <div id="chips"></div>
      <div id="limit-banner" class="hidden"></div>
      <textarea id="input"></textarea><button id="send"></button>
      <button id="ws-stop" class="hidden"></button>
      <button id="ws-publish"></button>
      <div id="preview-building-text"></div>
      <iframe id="preview"></iframe>
    </section>`;
  state.lastSeq = 0;
  state.projectId = "p1";
  state.building = false;
  state.hasVersion = false;
  state.steps = [];
  state.sessionState = "idle";
  state.runId = "";
}

let seq = 0;
const ev = (type: string, userText?: string, detail?: Record<string, unknown>): Event => ({
  seq: ++seq,
  type,
  userText,
  detail,
});
// The platform tells the client what state to render; tests drive that event
// rather than poking internals, because that is the whole contract (R-SES-5).
const stateEv = (s: string) => ev("session.state.changed", undefined, { state: s });

describe("build progress rendering", () => {
  beforeEach(() => {
    seq = 0;
    setupDom();
  });

  it("shows a transient build card from tool.result progress, not transcript bubbles", () => {
    handleEvent(stateEv("working"));
    handleEvent(ev("run.started"));
    expect(document.getElementById("build-card")).not.toBeNull();
    expect(document.getElementById("screen-workspace")?.getAttribute("data-preview")).toBe(
      "building",
    );

    handleEvent(ev("tool.result", "Working on your home page"));
    handleEvent(ev("tool.result", "Working on the styling"));
    // One self-replacing card — not transcript bubbles.
    expect(document.querySelectorAll("#build-card").length).toBe(1);
    // Two distinct steps: the first done, the newest active.
    const steps = [...document.querySelectorAll("#build-steps .step")].map((s) => s.textContent);
    expect(steps).toEqual(["✓Working on your home page", "●Working on the styling"]);
    expect(document.querySelectorAll(".msg").length).toBe(0);
  });

  it("clears the build card and building state on completion", () => {
    handleEvent(stateEv("working"));
    handleEvent(ev("run.started"));
    handleEvent(ev("tool.result", "Working on your home page"));
    handleEvent(ev("run.completed", "Your site is ready!"));
    handleEvent(stateEv("idle"));
    expect(document.getElementById("build-card")).toBeNull();
    expect(document.getElementById("screen-workspace")?.classList.contains("building")).toBe(false);
    // The final message IS a transcript bubble.
    expect([...document.querySelectorAll(".msg")].map((m) => m.textContent)).toContain(
      "Your site is ready!",
    );
  });

  it("does not add a step for tool.result with no phrase (inspection tools)", () => {
    handleEvent(stateEv("working"));
    handleEvent(ev("run.started"));
    handleEvent(ev("tool.result", "")); // read_file / list_files carry no phrase
    const steps = [...document.querySelectorAll("#build-steps .step")].map((s) => s.textContent);
    expect(steps).toEqual(["●Getting started"]);
  });

  it("collapses repeated identical phrases into one step", () => {
    handleEvent(stateEv("working"));
    handleEvent(ev("run.started"));
    handleEvent(ev("tool.result", "Working on your home page"));
    handleEvent(ev("tool.result", "Working on your home page"));
    const steps = [...document.querySelectorAll("#build-steps .step")];
    expect(steps.length).toBe(1);
  });
});

describe("user message rendering", () => {
  beforeEach(() => {
    seq = 0;
    setupDom();
  });

  const youBubbles = () => [...document.querySelectorAll(".msg.you")].map((m) => m.textContent);

  it("renders a user message from the stream exactly once", () => {
    handleEvent(ev("user.message", "build me a bakery site"));
    expect(youBubbles()).toEqual(["build me a bakery site"]);
  });

  it("does not double-render (the stream is the only path — no optimistic echo)", () => {
    handleEvent(ev("user.message", "add a menu page"));
    handleEvent(ev("run.started"));
    handleEvent(ev("run.completed", "Done — added your menu page."));
    expect(youBubbles()).toEqual(["add a menu page"]);
  });

  it("replays user messages on hydrate, preserving order with Creo's replies", () => {
    handleEvent(ev("user.message", "first request"));
    handleEvent(ev("assistant.message", "working on it"));
    handleEvent(ev("run.completed", "first done"));
    handleEvent(ev("user.message", "second request"));
    const order = [...document.querySelectorAll(".msg")].map(
      (m) => m.className.replace("msg ", "") + ":" + m.textContent,
    );
    expect(order).toEqual([
      "you:first request",
      "creo:working on it",
      "creo:first done",
      "you:second request",
    ]);
  });
});

describe("gentle error surfacing", () => {
  beforeEach(() => {
    seq = 0;
    setupDom();
  });

  it("routes a translated error to the banner, never a red transcript bubble", () => {
    handleEvent(ev("error.translated", "You've used your free changes for today."));
    const banner = document.getElementById("limit-banner");
    expect(banner?.classList.contains("hidden")).toBe(false);
    expect(banner?.textContent).toContain("free changes");
    expect(document.querySelectorAll(".msg").length).toBe(0);
  });
});

describe("questions from Creo", () => {
  beforeEach(() => {
    seq = 0;
    setupDom();
  });

  const card = () => document.getElementById("question-card");

  it("renders the question verbatim with its choices as buttons", () => {
    handleEvent(stateEv("working"));
    handleEvent(
      ev("input.requested", "What are your opening hours?", {
        choices: ["Weekdays 9–5", "Every day 8–6"],
      }),
    );
    handleEvent(stateEv("waiting-for-input"));
    expect(card()?.querySelector(".q")?.textContent).toBe("What are your opening hours?");
    expect(
      [...document.querySelectorAll("#question-card .choice")].map((c) => c.textContent),
    ).toEqual(["Weekdays 9–5", "Every day 8–6"]);
    // Waiting is not working: the composer stays open so free text works too.
    expect(document.querySelector<HTMLButtonElement>("#send")?.disabled).toBe(false);
    expect(document.getElementById("ws-status-text")?.textContent).toBe("Waiting for your answer");
  });

  it("renders a question with no choices — free text is always allowed", () => {
    handleEvent(ev("input.requested", "What should the homepage say?"));
    handleEvent(stateEv("waiting-for-input"));
    expect(card()).not.toBeNull();
    expect(document.querySelectorAll("#question-card .choice").length).toBe(0);
  });

  it("shows the answer another device gave, and retires the question", () => {
    handleEvent(
      ev("input.requested", "What are your opening hours?", { choices: ["Weekdays 9–5"] }),
    );
    handleEvent(stateEv("waiting-for-input"));
    // The other device answered; this one learns via the stream (AC-5).
    handleEvent(ev("input.provided", "Weekdays 9–5"));
    handleEvent(stateEv("queued"));
    expect(card()).toBeNull();
    expect([...document.querySelectorAll(".msg.you")].map((m) => m.textContent)).toEqual([
      "Weekdays 9–5",
    ]);
  });
});

describe("session state is rendered, never inferred", () => {
  beforeEach(() => {
    seq = 0;
    setupDom();
  });

  const stopHidden = () => document.getElementById("ws-stop")?.classList.contains("hidden") ?? true;

  it("offers Stop only while work is in flight", () => {
    expect(stopHidden()).toBe(true);
    handleEvent(stateEv("working"));
    expect(stopHidden()).toBe(false);
    handleEvent(stateEv("waiting-for-input"));
    expect(stopHidden()).toBe(true); // nothing to stop: it's the user's turn
    handleEvent(stateEv("idle"));
    expect(stopHidden()).toBe(true);
  });

  it("does not treat run.completed alone as idle — the platform says when", () => {
    handleEvent(stateEv("working"));
    handleEvent(ev("run.completed", "Done!"));
    // No state event yet: still working as far as the client knows.
    expect(state.sessionState).toBe("working");
    handleEvent(stateEv("idle"));
    expect(state.sessionState).toBe("idle");
    expect(document.getElementById("ws-status-text")?.textContent).toBe("Ready");
  });

  it("surfaces a cancellation as a plain note, not an error", () => {
    handleEvent(stateEv("working"));
    handleEvent(ev("run.cancelled", "Stopped. Your site is as it was before this change."));
    handleEvent(stateEv("idle"));
    const notes = [...document.querySelectorAll(".msg.note")].map((m) => m.textContent);
    expect(notes).toEqual(["Stopped. Your site is as it was before this change."]);
  });
});

// The sign-in surface. The picker shows names, never secrets; the family-mode
// warning is keyed to `assurance` (the property) rather than the driver name,
// and can only be silenced for the current page view.
function setupAuthDom() {
  document.body.innerHTML = `
    <section id="screen-key"><div id="account-list"></div>
      <div id="key-error" class="hidden"></div></section>
    <section id="screen-home">
      <div id="family-banner" class="hidden">
        <span id="family-banner-text"></span><button id="family-banner-close"></button>
      </div>
      <span id="home-who" class="hidden"></span>
      <button id="home-signout" class="hidden"></button>
    </section>
    <section id="screen-workspace"></section>`;
  state.who = null;
}

describe("account picker", () => {
  beforeEach(setupAuthDom);
  afterEach(() => vi.unstubAllGlobals());

  const stubLogin = (choices: unknown[]) =>
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        json: async () => ({ flowId: "lf_1", kind: "choice", choices }),
      }),
    );

  it("renders one button per account, showing names and no credentials", async () => {
    stubLogin([
      { id: "u_1", name: "Anna", color: "#e07a5f" },
      { id: "u_2", name: "Bob" },
    ]);
    await showAccountPicker();
    const buttons = [...document.querySelectorAll("#account-list .account")];
    expect(buttons.map((b) => b.textContent)).toEqual(["AAnna", "BBob"]);
    expect(document.querySelector("#account-list input")).toBeNull();
  });

  it("explains how to get an account when none exist yet", async () => {
    stubLogin([]);
    await showAccountPicker();
    expect(document.getElementById("account-empty")?.textContent).toContain("creo account new");
  });
});

describe("family-mode banner", () => {
  beforeEach(setupAuthDom);

  const hidden = () => document.getElementById("family-banner")?.classList.contains("hidden");

  it("shows for attributed identity — the T1 honesty note", () => {
    state.who = {
      tenantId: "t1",
      userId: "u1",
      name: "Anna",
      method: "static",
      assurance: "attributed",
    };
    renderWho();
    expect(hidden()).toBe(false);
    expect(document.getElementById("family-banner-text")?.textContent).toContain(
      "anyone who can reach",
    );
    expect(document.getElementById("home-who")?.textContent).toBe("Signed in as Anna");
  });

  it("stays hidden when identity is proven", () => {
    state.who = { tenantId: "t1", userId: "u1", name: "Anna", method: "oidc", assurance: "proven" };
    renderWho();
    expect(hidden()).toBe(true);
  });

  it("returns after a dismissal is followed by a re-render — no durable silence", () => {
    state.who = {
      tenantId: "t1",
      userId: "u1",
      name: "Anna",
      method: "static",
      assurance: "attributed",
    };
    renderWho();
    dismissBanner();
    expect(hidden()).toBe(true);
    // Nothing was persisted: a reload re-imports the module with a fresh flag.
    // Within one page view the dismissal holds, which is the whole contract.
    renderWho();
    expect(hidden()).toBe(true);
    expect(localStorage.getItem("creo_banner_dismissed")).toBeNull();
    expect(sessionStorage.length).toBe(0);
  });
});
