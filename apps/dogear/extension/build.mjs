#!/usr/bin/env node
// Builds the Dogear browser extension into dist/chrome and dist/firefox.
//
//   node extension/build.mjs          (from apps/dogear)
//   node build.mjs                    (from extension/)
//
// Steps: compile src/*.ts with tsc (fetched through `pnpm dlx`, so no install;
// the workspace override pins the version),
// copy static files and icons, then write a per-browser manifest.json.
// Chrome needs background.service_worker; Firefox MV3 needs background.scripts
// plus a gecko id, so the two targets differ only in the manifest.

import { execFileSync } from 'node:child_process';
import {
  cpSync,
  existsSync,
  mkdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = dirname(fileURLToPath(import.meta.url));
const dist = join(root, 'dist');
const tscOut = join(dist, '.tsc');

rmSync(dist, { recursive: true, force: true });

execFileSync(
  'pnpm',
  ['dlx', '--package=typescript', 'tsc', '-p', join(root, 'tsconfig.json')],
  {
    stdio: 'inherit',
    cwd: root,
  }
);

const base = JSON.parse(
  readFileSync(join(root, 'static', 'manifest.json'), 'utf8')
);

const targets = {
  chrome: {
    ...base,
    minimum_chrome_version: '116',
    background: { service_worker: 'js/background.js', type: 'module' },
  },
  firefox: {
    ...base,
    background: { scripts: ['js/background.js'], type: 'module' },
    browser_specific_settings: {
      gecko: {
        id: 'extension@dogear.fyi',
        strict_min_version: '128.0',
        data_collection_permissions: {
          required: ['websiteActivity', 'websiteContent'],
        },
      },
    },
  },
};

for (const [name, manifest] of Object.entries(targets)) {
  const out = join(dist, name);
  mkdirSync(join(out, 'js'), { recursive: true });
  for (const file of ['popup.html', 'options.html', 'style.css']) {
    cpSync(join(root, 'static', file), join(out, file));
  }
  cpSync(join(root, 'static', 'icons'), join(out, 'icons'), {
    recursive: true,
  });
  for (const js of ['api.js', 'popup.js', 'options.js', 'background.js']) {
    const src = join(tscOut, js);
    if (existsSync(src) === false) throw new Error(`tsc did not produce ${js}`);
    cpSync(src, join(out, 'js', js));
  }
  writeFileSync(
    join(out, 'manifest.json'),
    `${JSON.stringify(manifest, null, 2)}\n`
  );
  // Fail the build if any icon path in the manifest points at a missing file.
  for (const p of Object.values(manifest.icons)) {
    if (existsSync(join(out, p)) === false)
      throw new Error(`${name}: missing ${p}`);
  }
  console.log(`built ${join('dist', name)}`);
}

rmSync(tscOut, { recursive: true, force: true });
