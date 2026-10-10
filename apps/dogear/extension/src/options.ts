import { api, DEFAULT_BASE, loadSettings, normalizeBase } from './api.js';

const tokenInput = document.getElementById('token') as HTMLInputElement;
const baseInput = document.getElementById('base') as HTMLInputElement;
const form = document.getElementById('options-form') as HTMLFormElement;
const statusBox = document.getElementById('status') as HTMLDivElement;
const tokensLink = document.getElementById('tokens-link') as HTMLAnchorElement;

const LOCAL_ORIGIN_PATTERN = 'http://localhost/*';

function say(text: string, ok = true): void {
  statusBox.className = ok ? 'ok' : 'error';
  statusBox.textContent = text;
}

// Validates the base URL synchronously. Only the production origin or
// http://localhost (any port) is allowed, matching the manifest's host and
// optional host permissions.
function parseBase(raw: string): string | null {
  let origin: string;
  try {
    origin = normalizeBase(raw);
  } catch {
    return null;
  }
  if (origin === DEFAULT_BASE) return origin;
  const u = new URL(origin);
  if (u.protocol === 'http:' && u.hostname === 'localhost') return origin;
  return null;
}

async function save(base: string, granted: Promise<boolean>): Promise<void> {
  if (!(await granted)) {
    say(
      'Access to localhost was not granted, so the base URL was not changed.',
      false
    );
    return;
  }
  const token = tokenInput.value.trim();
  if (token !== '' && !token.startsWith('afk_')) {
    say('Tokens start with afk_.', false);
    return;
  }
  await chrome.storage.local.set({ token, base });
  tokensLink.href = `${base}/settings/tokens`;
  if (token === '') {
    say('Saved. Add a token to post.');
    return;
  }
  const me = await api<{ username: string; trust_level: number }>(
    { token, base },
    'GET',
    '/me'
  );
  if (me.ok && me.data !== undefined)
    say(
      `Saved. Signed in as ${me.data.username} (trust level ${me.data.trust_level}).`
    );
  else
    say(
      `Saved, but the token check failed: ${me.error?.message ?? `HTTP ${me.status}`}`,
      false
    );
}

form.addEventListener('submit', (e) => {
  e.preventDefault();
  const base = parseBase(baseInput.value);
  if (!base) {
    say(
      'Base URL must be https://dogear.fyi or http://localhost:<port>.',
      false
    );
    return;
  }
  // permissions.request must run synchronously inside the click/submit
  // gesture, so it is started here before any await.
  const granted =
    base === DEFAULT_BASE
      ? Promise.resolve(true)
      : chrome.permissions.request({ origins: [LOCAL_ORIGIN_PATTERN] });
  void save(base, granted);
});

void loadSettings().then((s) => {
  tokenInput.value = s.token;
  baseInput.value = s.base;
  tokensLink.href = `${s.base}/settings/tokens`;
});
