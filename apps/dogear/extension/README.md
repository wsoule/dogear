# Dogear browser extension

Chrome and Firefox Manifest V3 extension. The toolbar popup posts the current
page to Dogear with a one-line reason. A context-menu item ("Post link to
Dogear") posts a right-clicked link. Plain TypeScript, no framework, no content
scripts.

## Build

```bash
node extension/build.mjs   # from apps/dogear
```

The script compiles `src/*.ts` with `pnpm dlx --package=typescript tsc` (no
install needed), then writes `dist/chrome/` and `dist/firefox/`. The two targets
differ only in `manifest.json`: Chrome uses `background.service_worker`, and
Firefox uses `background.scripts` plus a `browser_specific_settings.gecko` id.

WebExtension types come from the hand-written `src/chrome.d.ts`, which covers
only the APIs the extension calls.

## Load unpacked

- **Chrome**: open `chrome://extensions`, turn on Developer mode, click "Load
  unpacked", and pick `dist/chrome`.
- **Firefox**: open `about:debugging#/runtime/this-firefox`, click "Load
  Temporary Add-on", and pick `dist/firefox/manifest.json`.

Then open the extension's options and paste an `afk_` token from
`/settings/tokens` with only the `read` and `post` scopes.

## Permissions

- `activeTab`: reads the current tab's URL and title when you open the popup.
- `contextMenus`: adds the "Post link to Dogear" menu item.
- `storage`: holds the token, base URL, and the category cache (refreshed
  daily).
- `host_permissions`: only `https://dogear.fyi/*`.
- `optional_host_permissions`: only `http://localhost/*`. The extension asks for
  it when you set a localhost base URL in options for local development. No
  other origins are accepted.

Requests send `X-Dogear-Client: dogear-extension/1.0`. Firefox also honours the
`User-Agent` override, but Chrome drops it from `fetch`, so the server should
read the custom header too.

## Icons

`static/icons/icon.svg` is the source mark: a paper square with a single
diagonal accent stroke across the top-right corner. The PNGs were rasterized
with ImageMagick draw primitives because its SVG renderer drops the stroke
colour:

```bash
for s in 16 32 48 128; do
  magick -size 1024x1024 xc:none -stroke none -fill '#ffffff' -draw "roundrectangle 64,64 960,960 48,48" \
    -stroke '#c2410c' -strokewidth 80 -draw "line 592,64 960,432" \
    -fill none -stroke '#111111' -strokewidth 64 -draw "roundrectangle 64,64 960,960 48,48" \
    -filter Lanczos -resize ${s}x${s} PNG32:static/icons/icon-$s.png
done
```

## Store submission notes

- **Chrome Web Store**: zip the contents of `dist/chrome` (with `manifest.json`
  at the zip root). For each permission, give the justification from the list
  above. The extension runs no remote code. Data use: the URLs and titles you
  choose to post are sent to dogear.fyi.
- **Firefox AMO**: zip the contents of `dist/firefox`. Upload the source as well
  (`src/`, `static/`, `build.mjs`, `tsconfig.json`), because the shipped JS is
  compiled from TypeScript. The manifest declares `data_collection_permissions`
  with `websiteActivity` and `websiteContent`. Check those category names
  against current AMO docs before you submit.
- Bump `version` in `static/manifest.json` for each release.
