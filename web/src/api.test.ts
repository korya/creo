import { describe, it, expect, vi, beforeEach } from "vitest";
import { Api } from "./api";

describe("Api", () => {
  beforeEach(() => vi.restoreAllMocks());

  function mockFetch(status: number, body: unknown) {
    return vi.fn().mockResolvedValue({
      ok: status < 400,
      status,
      statusText: "",
      json: async () => body,
    } as Response);
  }

  it("creates a project", async () => {
    const f = mockFetch(201, { id: "p1", name: "site", sessionId: "s1" });
    vi.stubGlobal("fetch", f);
    const api = new Api("http://x");
    const p = await api.createProject("site");
    expect(p.sessionId).toBe("s1");
    const [url, opts] = f.mock.calls[0];
    expect(url).toBe("http://x/v1/projects");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual({ name: "site" });
  });

  it("sends the idempotency key and bearer token", async () => {
    const f = mockFetch(202, { runId: "r1", deduped: false });
    vi.stubGlobal("fetch", f);
    const api = new Api("", "creo_abc");
    await api.sendMessage("s1", "build me a site", "key-1");
    const [url, opts] = f.mock.calls[0];
    expect(url).toBe("/v1/sessions/s1/messages");
    expect(opts.headers["Idempotency-Key"]).toBe("key-1");
    expect(opts.headers["Authorization"]).toBe("Bearer creo_abc");
  });

  it("surfaces the server error message", async () => {
    vi.stubGlobal("fetch", mockFetch(404, { error: "unknown session" }));
    const api = new Api();
    await expect(api.sendMessage("s1", "x", "k")).rejects.toThrow("404: unknown session");
  });

  it("publishes and returns the live url", async () => {
    vi.stubGlobal("fetch", mockFetch(200, { url: "http://sites/p1/", versionId: "v1" }));
    const api = new Api();
    const res = await api.publish("p1");
    expect(res.url).toBe("http://sites/p1/");
  });

  it("streams events over EventSource and unsubscribes", () => {
    const handlers: Record<string, (e: MessageEvent) => void> = {};
    const close = vi.fn();
    class FakeES {
      onmessage: ((e: MessageEvent) => void) | null = null;
      constructor(public url: string) {
        handlers["es"] = (e) => this.onmessage?.(e);
      }
      close = close;
    }
    vi.stubGlobal("EventSource", FakeES as unknown as typeof EventSource);

    const api = new Api("", "creo_tok");
    const seen: number[] = [];
    const unsub = api.streamEvents("s1", 5, (e) => seen.push(e.seq));
    handlers["es"]({ data: JSON.stringify({ seq: 6, type: "assistant.message" }) } as MessageEvent);
    handlers["es"]({ data: ": heartbeat" } as MessageEvent); // ignored
    expect(seen).toEqual([6]);
    unsub();
    expect(close).toHaveBeenCalled();
  });
});
