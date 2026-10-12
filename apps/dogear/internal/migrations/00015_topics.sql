-- +goose Up
-- Topics are broad, any-medium subjects for discovery (AI, Design, History),
-- separate from the developer-heavy categories that came from awesome lists.
-- An item can be in several topics. Dogear files items into topics itself:
-- by keywords, and by meaning when embeddings are available. description is
-- what an item's embedding is compared against.
CREATE TABLE topics (
  id          SERIAL PRIMARY KEY,
  slug        TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL,
  description TEXT NOT NULL,
  -- Postgres regular expression matched case-insensitively against an
  -- item's title, blurb, and creator.
  pattern     TEXT NOT NULL,
  sort_order  INT NOT NULL DEFAULT 0
);

CREATE TABLE item_topics (
  item_id  BIGINT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  topic_id INT NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
  -- how it was filed: 'keyword' or 'meaning'
  source   TEXT NOT NULL CHECK (source IN ('keyword','meaning')),
  PRIMARY KEY (item_id, topic_id)
);
CREATE INDEX item_topics_topic ON item_topics (topic_id, item_id);

-- When an item last went through keyword filing, so each hourly pass only
-- looks at new items.
ALTER TABLE items ADD COLUMN topics_filed_at TIMESTAMPTZ;
CREATE INDEX items_topics_todo ON items (id) WHERE status = 'live' AND topics_filed_at IS NULL;

INSERT INTO topics (slug, name, description, pattern, sort_order) VALUES
('ai', 'AI', 'Artificial intelligence: machine learning, neural networks, large language models, deep learning, AI research, and AI tools.',
  '\m(ai|a\.i\.|artificial intelligence|machine learning|deep learning|neural net(work)?s?|llms?|large language models?|gpt[-0-9a-z]*|chatgpt|transformers?|diffusion models?|reinforcement learning|computer vision|nlp|natural language processing|pytorch|tensorflow|langchain|openai|anthropic|claude)\M', 1),
('programming', 'Programming', 'Software development: programming languages, coding, libraries and frameworks, software engineering and architecture.',
  '\m(programming|coding|software engineering|developers?|compilers?|algorithms?|data structures|refactoring|golang|rust|python|javascript|typescript|haskell|elixir|clojure)\M', 2),
('design', 'Design', 'Design: visual and graphic design, UI and UX, typography, product design, illustration, and creative tools.',
  '\m(design(ers?)?|typography|typefaces?|fonts?|ui/ux|ux|user experience|user interface|figma|illustrations?|icons?|color palettes?)\M', 3),
('science', 'Science', 'Science: physics, biology, chemistry, astronomy and space, neuroscience, and research explained.',
  '\m(science|scientific|physics|quantum|biology|chemistry|astronomy|astrophysics|space|cosmology|neuroscience|evolution|genetics|climate)\M', 4),
('math', 'Math', 'Mathematics: proofs, geometry, calculus, linear algebra, probability, statistics, and math explained visually.',
  '\m(math(s|ematics|ematical)?|calculus|algebra|geometry|topology|probability|statistics|theorem|proofs?|number theory)\M', 5),
('business', 'Business', 'Business, startups, and economics: founding companies, strategy, marketing, investing, and how markets work.',
  '\m(business|startups?|founders?|entrepreneurs?(hip)?|venture capital|vc|economics|economy|marketing|strategy|investing|finance|saas)\M', 6),
('productivity', 'Productivity', 'Productivity and work: focus, habits, note-taking, time management, writing things down, and tools for getting things done.',
  '\m(productivity|habits?|note[- ]taking|notes apps?|time management|getting things done|gtd|deep work|second brain|personal knowledge)\M', 7),
('history', 'History', 'History: past events, eras, people, and how the world came to be.',
  '\m((?<!git )history|historical|ancient|medieval|world war|empire|civilization|archaeology)\M', 8),
('philosophy', 'Philosophy', 'Philosophy and ideas: ethics, meaning, stoicism, consciousness, and big questions.',
  '\m(philosophy|philosophers?|ethics|stoic(ism)?|existential(ism)?|consciousness|epistemology|metaphysics)\M', 9),
('psychology', 'Psychology', 'Psychology and the mind: behavior, cognition, emotions, mental health, and how people think.',
  '\m(psychology|psychological|cognitive|cognition|behavio(u)?r(al)?|mental health|emotions?|mindset|therapy)\M', 10),
('health', 'Health', 'Health and fitness: exercise, nutrition, sleep, medicine, and wellbeing.',
  '\m(health(?![- ]?checks?)|fitness|exercise|workouts?|nutrition|diet|sleep|medicine|medical|wellbeing|longevity)\M', 11),
('art', 'Art & Culture', 'Art and culture: painting, photography, architecture, museums, and creative work.',
  '\m(artists?|artworks?|art history|digital art|generative art|painting|photography|photographers?|museums?|sculpture|pop culture)\M', 12),
('music', 'Music', 'Music: songs, albums, music theory, production, instruments, and musicians.',
  '\m(music|musicians?|songs?|albums?|music theory|synth(esizer)?s?|guitar|piano|audio production|daw)\M', 13),
('film', 'Film & TV', 'Film and television: movies, shows, documentaries, filmmaking, and video essays about them.',
  '\m(films?|movies?|cinema|filmmaking|documentar(y|ies)|television|tv shows?|screenwriting)\M', 14),
('games', 'Games', 'Games: video games, game design, game development, and board games.',
  '\m(games?|gaming|game design|game dev(elopment)?|video games?|board games?|indie games?)\M', 15),
('writing', 'Books & Writing', 'Books and writing: reading lists, essays, authors, the craft of writing, and literature.',
  '\m(books?|writing|writers?|authors?|essays?|novels?|literature|reading list|poetry)\M', 16);

-- +goose Down
DROP INDEX items_topics_todo;
ALTER TABLE items DROP COLUMN topics_filed_at;
DROP TABLE item_topics, topics;
