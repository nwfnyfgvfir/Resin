import { getStoredAuthToken, handleUnauthorized } from "../features/auth/auth-store";

const API_BASE_URL = import.meta.env.VITE_API_BASE_URL?.trim() ?? "";

type Primitive = string | number | boolean | null;
type JsonValue = Primitive | JsonValue[] | { [key: string]: JsonValue };

export type ApiErrorBody = {
  error?: {
    code?: string;
    message?: string;
  };
};

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly body: ApiErrorBody | null;

  constructor(status: number, code: string, message: string, body: ApiErrorBody | null) {
    super(message);
    this.status = status;
    this.code = code;
    this.body = body;
  }
}

type RequestOptions = {
  method?: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
  body?: JsonValue;
  auth?: boolean;
  token?: string;
  signal?: AbortSignal;
};

function buildURL(path: string): string {
  if (path.startsWith("http://") || path.startsWith("https://")) {
    return path;
  }
  return `${API_BASE_URL}${path}`;
}

async function parseErrorBody(response: Response): Promise<ApiErrorBody | null> {
  const contentType = response.headers.get("content-type") ?? "";
  if (!contentType.includes("application/json")) {
    return null;
  }

  try {
    return (await response.json()) as ApiErrorBody;
  } catch {
    return null;
  }
}

export async function apiRequest<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = "GET", body, auth = true, token, signal } = options;
  const headers = new Headers();

  if (body !== undefined) {
    headers.set("Content-Type", "application/json; charset=utf-8");
  }

  // Track whether the request authenticated with the stored session token (as
  // opposed to an explicit token, e.g. the login form's validation probe).
  // Only the former should trigger a global logout on 401.
  let usedStoredToken = false;
  if (auth) {
    const explicitToken = token?.trim() ?? "";
    const resolvedToken = explicitToken || getStoredAuthToken();
    if (resolvedToken) {
      headers.set("Authorization", `Bearer ${resolvedToken}`);
      usedStoredToken = explicitToken === "";
    }
  }

  const response = await fetch(buildURL(path), {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    signal,
  });

  if (!response.ok) {
    const parsed = await parseErrorBody(response);
    const code = parsed?.error?.code ?? "HTTP_ERROR";
    const message = parsed?.error?.message ?? response.statusText;
    // The stored admin token is no longer valid (e.g. RESIN_ADMIN_TOKEN was
    // rotated): clear the session and let the app return to the login page
    // instead of leaving the UI stuck on repeated API errors.
    if (response.status === 401 && usedStoredToken) {
      handleUnauthorized();
    }
    throw new ApiError(response.status, code, message, parsed);
  }

  if (response.status === 204) {
    return undefined as T;
  }

  const contentType = response.headers.get("content-type") ?? "";
  if (!contentType.includes("application/json")) {
    return undefined as T;
  }

  return (await response.json()) as T;
}
