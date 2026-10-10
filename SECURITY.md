# Security Policy

## Reporting a vulnerability

Please **do not** open a public issue for security problems.

- Use GitHub's [private vulnerability reporting](../../security/advisories/new)
  for this repository, or
- email **hello@dogear.fyi** with "security" in the subject.

Include what you found, how to reproduce it, and the impact you expect. You
should hear back within a few days. Please give us a reasonable chance to fix
the problem before disclosing it.

## Scope

In scope: the code in this repository and the production site. Particularly
interesting areas are authentication and sessions, API tokens and OAuth flows,
the SSRF-safe fetcher and image proxy, rate limits and trust rules, and anything
that could expose private lists or hidden items.

Out of scope: denial of service by volume, findings that need a compromised
device, and reports from automated scanners without a working proof of concept.

## Supported versions

Only the `main` branch, which is what runs in production.
