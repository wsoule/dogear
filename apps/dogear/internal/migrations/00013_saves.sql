-- +goose Up
-- A user's archive: one row per thing they dogeared, with their one note on
-- it. Lists become tags on top of the archive; tagging always implies a save.
CREATE TABLE saves (
  user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  item_id    BIGINT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  note       TEXT NOT NULL DEFAULT '' CHECK (char_length(note) <= 280),
  saved_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, item_id)
);
CREATE INDEX saves_recent ON saves (user_id, saved_at DESC, item_id DESC);
CREATE INDEX saves_item ON saves (item_id);

-- Everything in any list becomes a save. Per-list notes merge into one:
-- distinct non-empty notes in the order they were first added, joined with
-- " · " and cut to the 280-character limit. The one note shows on every tag,
-- so only notes from the item's most visible lists are merged (public over
-- unlisted over private); a note written on a private list never surfaces on
-- a public page. Skipped notes stay in list_items.note, which is kept.
INSERT INTO saves (user_id, item_id, note, saved_at, updated_at)
SELECT e.user_id, e.item_id, COALESCE(n.note, ''), e.saved_at, e.updated_at
FROM (
  SELECT l.user_id, li.item_id, min(li.added_at) AS saved_at, max(li.updated_at) AS updated_at
  FROM list_items li JOIN lists l ON l.id = li.list_id
  GROUP BY l.user_id, li.item_id
) e
LEFT JOIN (
  SELECT user_id, item_id, left(string_agg(note, ' · ' ORDER BY first_added), 280) AS note
  FROM (
    SELECT r.user_id, r.item_id, r.note, min(r.added_at) AS first_added
    FROM (
      SELECT l.user_id, li.item_id, li.note, li.added_at,
        CASE l.visibility WHEN 'public' THEN 0 WHEN 'unlisted' THEN 1 ELSE 2 END AS rank,
        min(CASE l.visibility WHEN 'public' THEN 0 WHEN 'unlisted' THEN 1 ELSE 2 END)
          OVER (PARTITION BY l.user_id, li.item_id) AS best
      FROM list_items li JOIN lists l ON l.id = li.list_id
    ) r
    WHERE r.note <> '' AND r.rank = r.best
    GROUP BY r.user_id, r.item_id, r.note
  ) d
  GROUP BY user_id, item_id
) n ON n.user_id = e.user_id AND n.item_id = e.item_id;

-- A public Dogeared list was the archive; its entries are now plain saves.
-- Keep it as an ordinary tag when it is synced with loggit, or when it was
-- unlisted or private, so its items stay hidden from the public archive.
DELETE FROM lists l
WHERE l.slug = 'dogeared' AND l.visibility = 'public'
  AND NOT EXISTS (SELECT 1 FROM list_links k WHERE k.list_id = l.id);

-- +goose Down
-- Saves that are in no list go back into a public Dogeared list.
INSERT INTO lists (user_id, slug, title, description, visibility, sort_order)
SELECT DISTINCT s.user_id, 'dogeared', 'Dogeared', 'Things I folded the corner on.', 'public', -1
FROM saves s
WHERE NOT EXISTS (SELECT 1 FROM lists l WHERE l.user_id = s.user_id AND l.slug = 'dogeared');

INSERT INTO list_items (list_id, item_id, note, position, added_at)
SELECT l.id, s.item_id, s.note,
  (row_number() OVER (PARTITION BY l.id ORDER BY s.saved_at))::int - 1, s.saved_at
FROM saves s JOIN lists l ON l.user_id = s.user_id AND l.slug = 'dogeared'
WHERE NOT EXISTS (
  SELECT 1 FROM list_items li JOIN lists l2 ON l2.id = li.list_id
  WHERE l2.user_id = s.user_id AND li.item_id = s.item_id)
ON CONFLICT DO NOTHING;

-- The save's note goes back onto public entries and empty ones; notes on
-- unlisted and private lists were never merged, so they keep their own.
UPDATE list_items li SET note = s.note
FROM lists l, saves s
WHERE l.id = li.list_id AND s.user_id = l.user_id AND s.item_id = li.item_id
  AND (l.visibility = 'public' OR li.note = '');

DROP TABLE saves;
