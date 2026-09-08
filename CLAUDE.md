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
- Two instances run this code, and every change ships to both before it
  is called done. The host hub is the systemd unit `exe-hub`
  (`/www/exe-hub/exe-hub`, `sudo -n systemctl restart exe-hub`). The
  public one, https://hub.v2core.com, is the hub inside the exe `test` VM
  (`/home/dev/exe-hub`, published by exe expose): build it static
  (`CGO_ENABLED=0 go build -o exe-hub ./cmd/exe-hub`, arm64 like the
  host), `scp -P 2222 exe-hub test@127.0.0.1:/home/dev/exe-hub/exe-hub.new`
  through exe's SSH gate, then inside the VM `mv exe-hub.new exe-hub` and
  `sudo systemctl restart exe-hub` (user `dev` has passwordless sudo).
  Verify with curl against https://hub.v2core.com/.
