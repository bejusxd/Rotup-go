import type { ApiError, System } from "./types";

export class RequestError extends Error {
  constructor(
    public readonly detail: ApiError,
    public readonly status: number,
  ) {
    super(detail.userMessage);
  }
}

let token: string | undefined;
let bootstrap: Promise<System> | undefined;

// Secrets and response data are never written to browser storage or logs.
export async function request<T>(
  path: string,
  init: RequestInit = {},
): Promise<T> {
  let response: Response;
  try {
    response = await fetch(`/api/v1${path}`, {
      ...init,
      credentials: "same-origin",
      cache: "no-store",
    });
  } catch (error) {
    if (init.signal?.aborted) throw error;
    throw new RequestError(
      {
        code: "connection_lost",
        class: "UNAVAILABLE",
        message: "No response was received from rotupd.",
        userMessage:
          "Cannot reach ROTUP. If you started an operation, check History before retrying.",
      },
      0,
    );
  }
  const value = await response.json().catch(() => null);
  if (!response.ok) {
    const detail = value?.error;
    throw new RequestError(
      detail?.code && detail?.message && detail?.userMessage
        ? detail
        : {
            code: "invalid_response",
            class: "INTERNAL",
            message: `HTTP ${response.status}`,
            userMessage:
              "ROTUP returned an unexpected response. Check History before retrying an operation.",
          },
      response.status,
    );
  }
  if (value === null)
    throw new RequestError(
      {
        code: "invalid_response",
        class: "INTERNAL",
        message: "Expected a JSON response.",
        userMessage: "ROTUP returned an unreadable response.",
      },
      response.status,
    );
  return value as T;
}

export function loadSystem(): Promise<System> {
  // A single in-flight request avoids racing CSRF cookies during initial render.
  if (!bootstrap)
    bootstrap = request<System>("/system")
      .then((system) => {
        token = system.csrfToken;
        return system;
      })
      .finally(() => {
        bootstrap = undefined;
      });
  return bootstrap;
}

export async function mutate<T>(
  path: string,
  body: unknown = {},
  method = "POST",
): Promise<T> {
  if (!token) await loadSystem();
  try {
    return await request<T>(path, {
      method,
      headers: { "Content-Type": "application/json", "X-CSRF-Token": token! },
      body: JSON.stringify(body),
    });
  } catch (error) {
    // Never automatically retry a mutation: the operation may already exist.
    if (error instanceof RequestError && error.detail.code === "csrf_rejected")
      token = undefined;
    throw error;
  }
}
