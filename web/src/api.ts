// The sole API surface for the web client — the same public routes the CLI
// uses. If the client needs something not here, the platform is under-built,
// not the client (P2, headless core).

export interface Project {
  id: string;
  name: string;
  sessionId: string;
}

export interface Event {
  seq: number;
  type: string;
  userText?: string;
  runId?: string;
}

export interface PublishResult {
  url: string;
  versionId: string;
}

export interface Version {
  id: string;
  seq: number;
  parentId?: string;
  producedByEvent: string;
  createdAt: string;
}

export interface AccountChoice {
  id: string;
  name: string;
  color?: string;
}

export interface LoginFlow {
  flowId: string;
  kind: "choice" | "redirect";
  choices?: AccountChoice[];
  redirectUrl?: string;
}

// Principal is what the platform reports about the caller, however they
// authenticated. `assurance` — not the method name — is what the UI branches
// on: "attributed" means the account was picked, not proven, and the family
// -mode banner must be shown (components.md §11).
export interface Principal {
  userId?: string;
  tenantId: string;
  name?: string;
  method: string;
  assurance: "attributed" | "proven";
}

// ApiError carries the HTTP status as data. Callers branch on `status`, never
// on message text — `String(err)` on an Error is prefixed with "Error: ", so
// string sniffing silently never matches.
export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(`${status}: ${message}`);
    this.name = "ApiError";
  }
}

export function isUnauthorized(err: unknown): boolean {
  return err instanceof ApiError && err.status === 401;
}

export class Api {
  constructor(
    private base = "",
    private token = "",
  ) {}

  setToken(token: string) {
    this.token = token;
  }

  hasToken(): boolean {
    return this.token !== "";
  }

  private headers(extra: Record<string, string> = {}): Record<string, string> {
    const h: Record<string, string> = { "Content-Type": "application/json", ...extra };
    if (this.token) h["Authorization"] = `Bearer ${this.token}`;
    return h;
  }

  private async json<T>(
    method: string,
    path: string,
    body?: unknown,
    extra?: Record<string, string>,
  ): Promise<T> {
    const res = await fetch(this.base + path, {
      method,
      headers: this.headers(extra),
      // Session cookies are the human-login credential; same-origin keeps
      // them flowing on every call including the SSE stream.
      credentials: "same-origin",
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (!res.ok) {
      let msg = res.statusText;
      try {
        msg = (await res.json()).error ?? msg;
      } catch {
        /* non-JSON error */
      }
      throw new ApiError(res.status, msg);
    }
    return res.json() as Promise<T>;
  }

  // --- human login (the static driver's picker; oidc redirects at M5) ---

  beginLogin(): Promise<LoginFlow> {
    return this.json<LoginFlow>("POST", "/v1/auth/login/begin", {});
  }

  completeLogin(flowId: string, accountId: string): Promise<Principal> {
    return this.json<Principal>("POST", "/v1/auth/login/complete", {
      flowId,
      params: { account: accountId },
    });
  }

  me(): Promise<Principal> {
    return this.json<Principal>("GET", "/v1/auth/me");
  }

  async logout(): Promise<void> {
    await fetch(this.base + "/v1/auth/logout", {
      method: "POST",
      credentials: "same-origin",
    });
  }

  createProject(name: string): Promise<Project> {
    return this.json<Project>("POST", "/v1/projects", { name });
  }

  listProjects(): Promise<Project[]> {
    return this.json<Project[]>("GET", "/v1/projects");
  }

  sendMessage(
    sessionId: string,
    text: string,
    idempotencyKey: string,
  ): Promise<{ runId: string; deduped: boolean }> {
    return this.json(
      "POST",
      `/v1/sessions/${sessionId}/messages`,
      { text },
      { "Idempotency-Key": idempotencyKey },
    );
  }

  // Cursor-based replay of the whole session (used to hydrate on load).
  fetchEvents(sessionId: string, after = 0): Promise<Event[]> {
    return this.json<Event[]>(
      "GET",
      `/v1/sessions/${sessionId}/events?stream=false&after=${after}`,
    );
  }

  publish(projectId: string, versionId?: string): Promise<PublishResult> {
    return this.json<PublishResult>(
      "POST",
      `/v1/projects/${projectId}/publish`,
      versionId ? { versionId } : {},
    );
  }

  rollback(projectId: string): Promise<PublishResult> {
    return this.json<PublishResult>("POST", `/v1/projects/${projectId}/rollback`, {});
  }

  preview(projectId: string): Promise<{ url: string; versionId: string }> {
    return this.json("GET", `/v1/projects/${projectId}/preview`);
  }

  // Newest-first list of a project's saved versions (its history).
  versions(projectId: string): Promise<Version[]> {
    return this.json<Version[]>("GET", `/v1/projects/${projectId}/versions`);
  }

  // Live tail over SSE. Returns an unsubscribe. Browsers authenticated by
  // cookie need nothing extra — a same-origin EventSource sends it. Only the
  // operator token path needs the query param, since EventSource cannot set
  // an Authorization header. (T1 posture; tightened at T2.)
  streamEvents(sessionId: string, after: number, onEvent: (e: Event) => void): () => void {
    let url = `${this.base}/v1/sessions/${sessionId}/events?after=${after}`;
    if (this.token) url += `&token=${encodeURIComponent(this.token)}`;
    const es = new EventSource(url, { withCredentials: true });
    es.onmessage = (m) => {
      try {
        onEvent(JSON.parse(m.data) as Event);
      } catch {
        /* heartbeat or malformed */
      }
    };
    return () => es.close();
  }
}
