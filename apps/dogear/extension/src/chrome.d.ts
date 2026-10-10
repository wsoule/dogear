// Minimal hand-written declarations for the WebExtension APIs this extension
// uses. Kept small on purpose so the build needs no @types/chrome install.
// Firefox exposes the same `chrome.*` namespace for these calls.

declare namespace chrome {
  namespace runtime {
    const id: string;
    const lastError: { message?: string } | undefined;
    function getURL(path: string): string;
    function openOptionsPage(): Promise<void>;
    const onInstalled: {
      addListener(cb: (details: { reason: string }) => void): void;
    };
  }

  namespace storage {
    interface StorageArea {
      get(keys?: string | string[] | null): Promise<Record<string, unknown>>;
      set(items: Record<string, unknown>): Promise<void>;
      remove(keys: string | string[]): Promise<void>;
    }
    const local: StorageArea;
  }

  namespace tabs {
    interface Tab {
      id?: number;
      url?: string;
      title?: string;
      windowId: number;
    }
    function query(q: {
      active?: boolean;
      currentWindow?: boolean;
    }): Promise<Tab[]>;
    function create(props: { url: string }): Promise<Tab>;
  }

  namespace windows {
    function create(props: {
      url: string;
      type?: 'popup' | 'normal';
      width?: number;
      height?: number;
      focused?: boolean;
    }): Promise<{ id?: number }>;
  }

  namespace action {
    function openPopup(options?: { windowId?: number }): Promise<void>;
  }

  namespace contextMenus {
    interface OnClickData {
      menuItemId: string | number;
      linkUrl?: string;
      selectionText?: string;
      pageUrl?: string;
    }
    function create(
      props: { id: string; title: string; contexts: string[] },
      cb?: () => void
    ): void;
    function removeAll(cb?: () => void): void;
    const onClicked: {
      addListener(cb: (info: OnClickData, tab?: tabs.Tab) => void): void;
    };
  }

  namespace permissions {
    interface Permissions {
      origins?: string[];
      permissions?: string[];
    }
    function request(p: Permissions): Promise<boolean>;
    function contains(p: Permissions): Promise<boolean>;
  }
}
