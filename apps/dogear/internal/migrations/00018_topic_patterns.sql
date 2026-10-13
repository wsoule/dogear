-- +goose Up
-- Programming's keywords missed whole areas of software ("Redesigning source
-- control at Anthropic" matched nothing), so widen them and its description,
-- then mark every item unfiled so the next hourly pass re-files them all.
-- Filing only adds topics, so re-running it is safe.
UPDATE topics SET
  description = 'Software development: programming languages, coding, libraries and frameworks, software engineering and architecture, source control, developer tools, databases, and infrastructure.',
  pattern = '\m(programming|coding|software( engineering| development| design| architecture)?|developers?|compilers?|algorithms?|data structures|refactoring|source control|version control|git|monorepos?|code review|devops|ci/cd|open source|apis?|databases?|sql|postgres(ql)?|backend|frontend|web development|kubernetes|docker|golang|rust|python|javascript|typescript|haskell|elixir|clojure)\M'
WHERE slug = 'programming';
UPDATE items SET topics_filed_at = NULL WHERE topics_filed_at IS NOT NULL;

-- +goose Down
UPDATE topics SET
  description = 'Software development: programming languages, coding, libraries and frameworks, software engineering and architecture.',
  pattern = '\m(programming|coding|software engineering|developers?|compilers?|algorithms?|data structures|refactoring|golang|rust|python|javascript|typescript|haskell|elixir|clojure)\M'
WHERE slug = 'programming';
