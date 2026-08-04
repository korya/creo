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
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (!res.ok) {
      let msg = res.statusText;
      try {
        msg = (await res.json()).error ?? msg;
      } catch {
        /* non-JSON error */
      }
      throw new Error(`${res.status}: ${msg}`);
    }
    return res.json() as Promise<T>;
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

  // Live tail over SSE. Returns an unsubscribe. EventSource can't send an
  // Authorization header, so in authenticated mode the token rides as a query
  // param; --insecure dev mode needs neither. (T1 posture; tightened at T2.)
  streamEvents(sessionId: string, after: number, onEvent: (e: Event) => void): () => void {
    let url = `${this.base}/v1/sessions/${sessionId}/events?after=${after}`;
    if (this.token) url += `&token=${encodeURIComponent(this.token)}`;
    const es = new EventSource(url);
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
