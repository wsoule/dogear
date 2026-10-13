import {
  api,
  type ApiResult,
  type Category,
  getCategories,
  type Item,
  KINDS,
  type List,
  loadSettings,
  type Settings,
} from './api.js';

const PENDING_MAX_AGE_MS = 2 * 60 * 1000;

const $ = <T extends HTMLElement>(id: string): T =>
  document.getElementById(id) as T;

const form = $<HTMLFormElement>('post-form');
const urlInput = $<HTMLInputElement>('url');
const titleInput = $<HTMLInputElement>('title');
const blurbInput = $<HTMLTextAreaElement>('blurb');
const blurbCount = $<HTMLSpanElement>('blurb-count');
const categorySelect = $<HTMLSelectElement>('category');
const kindSelect = $<HTMLSelectElement>('kind');
const listSelect = $<HTMLSelectElement>('list');
const noteInput = $<HTMLInputElement>('note');
const submitButton = $<HTMLButtonElement>('submit');
const statusBox = $<HTMLDivElement>('status');

function option(value: string, label: string): HTMLOptionElement {
  const o = document.createElement('option');
  o.value = value;
  o.textContent = label;
  return o;
}

// Clears the status box and fills it with plain-text lines and optional nodes.
// Server text is always set through textContent, never parsed as HTML.
function showStatus(kind: 'ok' | 'error', ...parts: (string | Node)[]): void {
  statusBox.className = kind;
  statusBox.replaceChildren();
  for (const part of parts) {
    if (typeof part === 'string') {
      const p = document.createElement('div');
      p.textContent = part;
      statusBox.append(p);
    } else {
      statusBox.append(part);
    }
  }
}

function link(href: string, text: string): HTMLAnchorElement {
  const a = document.createElement('a');
  a.href = href;
  a.textContent = text;
  a.target = '_blank';
  a.rel = 'noopener';
  return a;
}

// Fills the category select with the tree; parents become optgroups because
// the API only accepts subcategory slugs.
function renderCategories(categories: Category[]): void {
  categorySelect.replaceChildren(option('', '(auto)'));
  for (const parent of categories) {
    const group = document.createElement('optgroup');
    group.label = parent.name;
    for (const child of parent.children ?? [])
      group.append(option(child.slug, child.name));
    if (group.children.length > 0) categorySelect.append(group);
  }
}

function renderLists(lists: List[]): void {
  listSelect.replaceChildren(option('', '(none)'));
  for (const l of lists)
    listSelect.append(option(l.slug, `${l.title} (${l.visibility})`));
}

// Prefills url/title from a context-menu click if one is waiting in storage,
// otherwise from the active tab (readable thanks to activeTab).
async function prefill(): Promise<void> {
  const { pending } = (await chrome.storage.local.get('pending')) as {
    pending?: { url: string; title?: string; at: number };
  };
  if (pending !== undefined && Date.now() - pending.at < PENDING_MAX_AGE_MS) {
    await chrome.storage.local.remove('pending');
    urlInput.value = pending.url;
    titleInput.value = pending.title ?? '';
    return;
  }
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (tab?.url && /^https?:/.test(tab.url)) {
    urlInput.value = tab.url;
    titleInput.value = (tab.title ?? '').slice(0, 120);
  }
}

function describeError(res: ApiResult<unknown>): string {
  const msg = res.error?.message ?? `HTTP ${res.status}`;
  if (res.status === 429) {
    return res.retryAfter ? `${msg} (retry in ${res.retryAfter}s)` : msg;
  }
  if (res.status === 401) return `${msg}. Check the token in options.`;
  return msg;
}

// Saves an existing item to the user's archive (with their note), and to
// the selected tag when one is picked. Used after a 409 duplicate, since
// the link is already on Dogear and only needs dogearing.
async function saveExisting(
  settings: Settings,
  itemId: number,
  button: HTMLButtonElement
): Promise<void> {
  button.disabled = true;
  const note = noteInput.value.trim();
  const res = await api<unknown>(
    settings,
    'PUT',
    `/me/saves/${itemId}`,
    note === '' ? {} : { note }
  );
  const slug = listSelect.value;
  const tagged =
    res.ok && slug !== ''
      ? await api<unknown>(
          settings,
          'PUT',
          `/me/lists/${encodeURIComponent(slug)}/items/${itemId}`,
          {}
        )
      : res;
  if (res.ok && tagged.ok) {
    showStatus(
      'ok',
      slug === ''
        ? 'Saved to your dogear.'
        : `Saved to your dogear, tagged #${slug}.`,
      link(`${settings.base}/i/${itemId}`, 'view item')
    );
  } else {
    button.disabled = false;
    showStatus('error', describeError(res.ok ? tagged : res));
  }
}

async function submit(settings: Settings): Promise<void> {
  const body: Record<string, string> = {
    url: urlInput.value.trim(),
    title: titleInput.value.trim(),
    blurb: blurbInput.value.trim(),
  };
  if (categorySelect.value !== '') body.category = categorySelect.value;
  if (kindSelect.value !== '') body.kind = kindSelect.value;
  if (listSelect.value !== '') body.list = listSelect.value;
  if (noteInput.value.trim() !== '') body.note = noteInput.value.trim();

  submitButton.disabled = true;
  showStatus('ok', 'posting…');
  const res = await api<Item>(settings, 'POST', '/items', body);
  submitButton.disabled = false;

  if (res.ok && res.data !== undefined) {
    const item = res.data;
    const msg =
      res.status === 202 || item.status === 'pending'
        ? 'Submitted. It is pending review before it goes live.'
        : 'Posted.';
    showStatus(
      'ok',
      msg,
      link(`${settings.base}/i/${item.id}`, `${settings.base}/i/${item.id}`)
    );
    submitButton.disabled = true;
    return;
  }

  const dupId = res.error?.item_id;
  if (res.status === 409 && dupId !== undefined && dupId > 0) {
    const id = dupId;
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.textContent = 'Already here: save it to your dogear';
    btn.addEventListener('click', () => void saveExisting(settings, id, btn));
    showStatus(
      'error',
      describeError(res),
      link(`${settings.base}/i/${id}`, 'existing item'),
      btn
    );
    return;
  }

  showStatus('error', describeError(res));
}

async function main(): Promise<void> {
  for (const k of KINDS) kindSelect.append(option(k, k));
  kindSelect.prepend(option('', '(auto)'));
  kindSelect.value = '';

  const openOptions = (e: Event) => {
    e.preventDefault();
    void chrome.runtime.openOptionsPage();
  };
  $('open-options').addEventListener('click', openOptions);
  $('setup-link').addEventListener('click', openOptions);

  blurbInput.addEventListener('input', () => {
    blurbCount.textContent = `${blurbInput.value.trim().length}/280`;
  });

  const settings = await loadSettings();
  await prefill();
  renderLists([]);

  if (settings.token === '') {
    $('no-token').hidden = false;
    submitButton.disabled = true;
  }

  const [categories, lists] = await Promise.all([
    getCategories(settings),
    settings.token !== ''
      ? api<{ lists: List[] }>(settings, 'GET', '/me/lists')
      : Promise.resolve<ApiResult<{ lists: List[] }>>({ ok: false, status: 0 }),
  ]);
  renderCategories(categories);
  if (lists.ok && lists.data !== undefined) renderLists(lists.data.lists);
  else if (lists.status === 401) showStatus('error', describeError(lists));

  form.addEventListener('submit', (e) => {
    e.preventDefault();
    if (!form.reportValidity()) return;
    void submit(settings);
  });
  (urlInput.value === '' ? urlInput : blurbInput).focus();
}

void main();
