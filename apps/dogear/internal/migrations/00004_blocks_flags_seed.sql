-- +goose Up
CREATE TABLE user_blocks (
  blocker_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  blocked_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (blocker_id, blocked_id)
);

CREATE TABLE mobile_login_codes (
  code_hash  BYTEA PRIMARY KEY,
  user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TIMESTAMPTZ NOT NULL,
  used_at    TIMESTAMPTZ
);

-- Account deletion must not be blocked by invite chains or a mod's history.
ALTER TABLE users DROP CONSTRAINT users_invited_by_fkey,
  ADD CONSTRAINT users_invited_by_fkey FOREIGN KEY (invited_by) REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE mod_actions ALTER COLUMN mod_id DROP NOT NULL,
  DROP CONSTRAINT mod_actions_mod_id_fkey,
  ADD CONSTRAINT mod_actions_mod_id_fkey FOREIGN KEY (mod_id) REFERENCES users(id) ON DELETE SET NULL;

-- Reasons a mod should look at an item (classifier holds, injection patterns).
ALTER TABLE items ADD COLUMN flags TEXT[] NOT NULL DEFAULT '{}';

-- Accounts flagged by nightly ring detection, shown in /mod.
CREATE TABLE user_flags (
  user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  reason     TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  cleared_at TIMESTAMPTZ,
  PRIMARY KEY (user_id, reason)
);

-- Demotion looks at recent removals, so record when an item was removed.
ALTER TABLE items ADD COLUMN removed_at TIMESTAMPTZ;

INSERT INTO categories (slug, name, sort_order) VALUES
  ('hardware-and-devices', 'Hardware and Devices', 900),
  ('apps-and-software', 'Apps and Software', 901),
  ('productivity-and-work', 'Productivity and Work', 902),
  ('learning', 'Learning', 903),
  ('misc', 'Misc', 999)
ON CONFLICT (slug) DO NOTHING;

INSERT INTO categories (slug, name, parent_id, sort_order)
SELECT v.slug, v.name, p.id, v.ord
FROM (VALUES
  ('hardware-and-devices', 'computers', 'Computers', 1),
  ('hardware-and-devices', 'phones', 'Phones', 2),
  ('hardware-and-devices', 'audio', 'Audio', 3),
  ('hardware-and-devices', 'peripherals', 'Peripherals', 4),
  ('hardware-and-devices', 'wearables', 'Wearables', 5),
  ('apps-and-software', 'mac', 'Mac', 1),
  ('apps-and-software', 'windows', 'Windows', 2),
  ('apps-and-software', 'linux-apps', 'Linux', 3),
  ('apps-and-software', 'ios', 'iOS', 4),
  ('apps-and-software', 'android', 'Android', 5),
  ('apps-and-software', 'web-apps', 'Web apps', 6),
  ('apps-and-software', 'command-line', 'Command line', 7),
  ('productivity-and-work', 'note-taking', 'Note-taking', 1),
  ('productivity-and-work', 'automation', 'Automation', 2),
  ('productivity-and-work', 'focus', 'Focus', 3),
  ('learning', 'learn-programming', 'Programming', 1),
  ('learning', 'learn-design', 'Design', 2),
  ('learning', 'learn-science', 'Science', 3),
  ('learning', 'learn-business', 'Business', 4),
  ('misc', 'unsorted', 'Unsorted', 99)
) AS v(parent, slug, name, ord)
JOIN categories p ON p.slug = v.parent
ON CONFLICT (slug) DO NOTHING;

INSERT INTO blocked_domains (domain, reason) VALUES
  ('bit.ly', 'shortener'), ('tinyurl.com', 'shortener'), ('t.co', 'shortener'),
  ('goo.gl', 'shortener'), ('ow.ly', 'shortener'), ('is.gd', 'shortener'),
  ('buff.ly', 'shortener'), ('cutt.ly', 'shortener'), ('rebrand.ly', 'shortener'),
  ('shorturl.at', 'shortener'), ('tiny.cc', 'shortener'), ('rb.gy', 'shortener')
ON CONFLICT DO NOTHING;

-- +goose Down
DELETE FROM categories WHERE parent_id IN (SELECT id FROM categories WHERE slug IN
  ('hardware-and-devices','apps-and-software','productivity-and-work','learning','misc'));
DELETE FROM categories WHERE slug IN
  ('hardware-and-devices','apps-and-software','productivity-and-work','learning','misc');
ALTER TABLE items DROP COLUMN removed_at, DROP COLUMN flags;
DROP TABLE user_flags, mobile_login_codes, user_blocks;
