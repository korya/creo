import { describe, it, expect, beforeEach } from "vitest";
import { state, handleEvent } from "./app";
import type { Event } from "./api";

// Minimal DOM the render path touches. app.ts's load-time init() is guarded on
// #send existing at import time (absent here), so importing has no side effects;
// we build the DOM afterwards and drive handleEvent directly.
function setupDom() {
  document.body.innerHTML = `
    <span id="status"></span>
    <div id="chat"><div id="log"></div>
      <textarea id="input"></textarea><button id="send"></button></div>
    <section id="preview-pane"><div id="preview-overlay"></div>
      <iframe id="preview"></iframe></section>
    <button id="publish"></button>`;
  state.lastSeq = 0;
  state.projectId = "p1"; // so publish enable/disable logic doesn't throw
}

let seq = 0;
const ev = (type: string, userText?: string): Event => ({ seq: ++seq, type, userText });

describe("build progress rendering", () => {
  beforeEach(() => {
    seq = 0;
    setupDom();
  });

  it("shows a transient activity line from tool.result progress, not the transcript", () => {
    handleEvent(ev("run.started"));
    expect(document.getElementById("activity")?.textContent).toBe("Getting started…");
    expect(document.getElementById("preview-pane")?.classList.contains("building")).toBe(true);

    handleEvent(ev("tool.result", "Working on your home page"));
    handleEvent(ev("tool.result", "Working on the styling"));
    // Single self-replacing element — not two transcript bubbles.
    expect(document.querySelectorAll("#activity").length).toBe(1);
    expect(document.getElementById("activity")?.textContent).toBe("Working on the styling…");
    expect(document.querySelectorAll(".msg").length).toBe(0);
  });

  it("clears the activity line and building state on completion", () => {
    handleEvent(ev("run.started"));
    handleEvent(ev("tool.result", "Working on your home page"));
    handleEvent(ev("run.completed", "Your site is ready!"));
    expect(document.getElementById("activity")).toBeNull();
    expect(document.getElementById("preview-pane")?.classList.contains("building")).toBe(false);
    // The final message IS a transcript bubble.
    expect([...document.querySelectorAll(".msg")].map((m) => m.textContent)).toContain("Your site is ready!");
  });

  it("ignores tool.result events with no phrase (inspection tools)", () => {
    handleEvent(ev("run.started"));
    handleEvent(ev("tool.result", "")); // read_file / list_files carry no phrase
    expect(document.getElementById("activity")?.textContent).toBe("Getting started…");
  });
});

describe("user message rendering (Finding 3)", () => {
  beforeEach(() => {
    seq = 0;
    setupDom();
  });

  const youBubbles = () =>
    [...document.querySelectorAll(".msg.you")].map((m) => m.textContent);

  it("renders a user message from the stream exactly once", () => {
    handleEvent(ev("user.message", "build me a bakery site"));
    expect(youBubbles()).toEqual(["build me a bakery site"]);
  });

  it("does not double-render (the stream is the only path — no optimistic echo)", () => {
    // Simulate the live flow: a single user.message event drives the one bubble.
    handleEvent(ev("user.message", "add a menu page"));
    handleEvent(ev("run.started"));
    handleEvent(ev("run.completed", "Done — added your menu page."));
    expect(youBubbles()).toEqual(["add a menu page"]);
  });

  it("replays user messages on hydrate, preserving order with Creo's replies", () => {
    // Hydrate = handleEvent over the full replayed log.
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
