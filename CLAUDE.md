# exe-hub

A standalone Go daemon: twitter-like social feed for exe nodes. SQLite
storage, ed25519-signed messages, IPFS embeds, optional Solana token gate.

**PLAN.md is the single source of truth for every project decision.**
Before implementing, check PLAN.md; when a design decision changes, update
PLAN.md first (or alongside the code) — code follows the plan, never the
other way around. If code and PLAN.md disagree, PLAN.md wins and the code
is the bug.

Notes:

- `config.json` is gitignored (its RPC URL embeds an API key). The
  committed shape lives in `config.example.json`.
