// Background worker: registers the "Post link to Dogear" context menu and
// hands the right-clicked link to the popup through chrome.storage.local.

const MENU_ID = 'dogear-post-link';

chrome.runtime.onInstalled.addListener(() => {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({
      id: MENU_ID,
      title: 'Post link to Dogear',
      contexts: ['link'],
    });
  });
});

// Opens the toolbar popup; if the browser refuses (older Chrome, or no focused
// window), falls back to popup.html in a small standalone window.
async function openPopup(windowId?: number): Promise<void> {
  try {
    await chrome.action.openPopup(
      windowId === undefined ? undefined : { windowId }
    );
  } catch {
    await chrome.windows.create({
      url: chrome.runtime.getURL('popup.html'),
      type: 'popup',
      width: 400,
      height: 560,
    });
  }
}

chrome.contextMenus.onClicked.addListener((info, tab) => {
  if (info.menuItemId !== MENU_ID || !info.linkUrl) return;
  // Start the storage write and the popup open in the same tick: Firefox only
  // allows openPopup while the click's user gesture is still active.
  void chrome.storage.local.set({
    pending: {
      url: info.linkUrl,
      title: (info.selectionText ?? '').slice(0, 120),
      at: Date.now(),
    },
  });
  void openPopup(tab?.windowId);
});
