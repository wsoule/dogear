// Shared settings and REST client for the popup and options page.

export const DEFAULT_BASE = 'https://dogear.fyi';
export const CLIENT = 'dogear-extension/1.0';
export const KINDS = [
  'site',
  'tool',
  'app',
  'book',
  'device',
  'video',
  'article',
  'other',
] as const;

const CATEGORY_TTL_MS = 24 * 60 * 60 * 1000;

export interface Settings {
  token: string;
  base: string;
}

export interface Category {
  slug: string;
  name: string;
  count?: number;
  children?: Category[];
}

export interface List {
  slug: string;
  title: string;
  description?: string;
  visibility: string;
  count?: number;
}

export interface Item {
  id: number;
  url: string;
  title: string;
  status: string;
}

export interface ApiError {
  code: string;
  message: string;
  item_id?: number;
}

// Result of one API call: either parsed JSON data or the server's error body,
// plus the HTTP status and Retry-After header so callers can explain 429s.
export interface ApiResult<T> {
  ok: boolean;
  status: number;
  data?: T;
  error?: ApiError;
  retryAfter?: string | null;
}

// Strips trailing slashes and any path so the base is always a bare origin.
export function normalizeBase(raw: string): string {
  const u = new URL(raw.trim() === '' ? DEFAULT_BASE : raw.trim());
  return u.origin;
}

export async function loadSettings(): Promise<Settings> {
  const s = await chrome.storage.local.get(['token', 'base']);
  return {
    token: typeof s.token === 'string' ? s.token : '',
    base: typeof s.base === 'string' && s.base !== '' ? s.base : DEFAULT_BASE,
  };
}

// Calls the Dogear REST API with the stored bearer token. Network failures are
// folded into an ApiResult with status 0 so the UI has one error path.
export async function api<T>(
  settings: Settings,
  method: string,
  path: string,
  body?: unknown
): Promise<ApiResult<T>> {
  const headers: Record<string, string> = {
    Accept: 'application/json',
    // Chrome silently drops a custom User-Agent from fetch; Firefox honours it.
    // X-Dogear-Client carries the same value so traffic stays traceable.
    'User-Agent': CLIENT,
    'X-Dogear-Client': CLIENT,
  };
  if (settings.token !== '') headers.Authorization = `Bearer ${settings.token}`;
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  let res: Response;
  try {
    res = await fetch(`${settings.base}/api/v1${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: 'omit',
    });
  } catch (err) {
    return {
      ok: false,
      status: 0,
      error: { code: 'network', message: `Network error: ${String(err)}` },
    };
  }
  let json: unknown = null;
  try {
    json = await res.json();
  } catch {
    json = null;
  }
  if (res.ok) return { ok: true, status: res.status, data: json as T };
  const errBody = (json as { error?: ApiError } | null)?.error;
  return {
    ok: false,
    status: res.status,
    error: errBody ?? { code: 'http', message: `HTTP ${res.status}` },
    retryAfter: res.headers.get('Retry-After'),
  };
}

// Returns the category tree, served from chrome.storage.local when the cached
// copy for this base URL is less than a day old.
export async function getCategories(settings: Settings): Promise<Category[]> {
  const key = `categories:${settings.base}`;
  const cached = (await chrome.storage.local.get(key))[key] as
    | { at: number; categories: Category[] }
    | undefined;
  if (cached !== undefined && Date.now() - cached.at < CATEGORY_TTL_MS)
    return cached.categories;
  const res = await api<{ categories: Category[] }>(
    settings,
    'GET',
    '/categories'
  );
  if (!res.ok || res.data === undefined) return cached?.categories ?? [];
  await chrome.storage.local.set({
    [key]: { at: Date.now(), categories: res.data.categories },
  });
  return res.data.categories;
}
