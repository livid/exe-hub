# exe-hub — plan

A standalone Go daemon providing a twitter-like social feed for exe nodes.
Storage is SQLite. Writes arrive as ed25519-signed messages; reads are public
HTTP. Embeds live in IPFS. Who may post is controlled by a JSON config —
open by default, optionally gated by holding an SPL token on Solana.

Status: design agreed, not yet built.

**This file is the single source of truth for the project design.** Update
it whenever a decision changes; code follows the plan, not the other way
around.

## Identity & signing

- Authors sign with the exe node's peer identity (`~/.exe/peer_ed25519`).
  No new key, no registration flow: the pubkey is the identity.
- **Domain separation:** every hub signature is over
  `"exe-hub:v1\n" + envelope bytes`. Peer-sync signatures always start with
  an HTTP method (`canonical()` in exe's `internal/peer/auth.go`), so the
  two protocols can never accept each other's signatures.
- **Sign bytes, not structures.** The client serializes the envelope once,
  signs those exact bytes, and sends `{envelope, sig}`. The hub verifies
  over the received bytes and stores them verbatim — no canonical-JSON
  re-serialization anywhere.
- Message ID = `sha256(envelope bytes)` → idempotent retries, free dedup.
- Profile ID = pubkey fingerprint, same scheme as exe node IDs
  (16 hex chars of sha256). Display names are non-unique; the fingerprint
  is the canonical handle. No name-squatting arbitration.

## Hub identity & deployment

- Anyone may run their own exe-hub; nothing in the design assumes a single
  shared instance.
- Each hub generates its **own ed25519 identity** on first start (e.g.
  `hub_ed25519` in its state dir, PKCS8 PEM like exe's peer key — but a
  distinct key, never a node's peer identity). Unused by the v1 API beyond
  being surfaced in a hub-info endpoint; it exists so a future
  **trust-aggregation** feature can let hubs identify and sign exchanges
  with each other. Hub signatures get their own domain prefix when that
  lands.

## Aggregation (semantics deferred — decisions so far)

Structural decisions are made now so v1 doesn't paint over them; the
replication mechanics come later:

- **"Trust" means manual admin curation, nothing more.** A hub aggregates
  only from peers its admin explicitly added via `peer.add`. There is no
  automatic peer discovery, no reputation system, and no transitive trust
  — a peer's peers are not implied.

- `allow_replication` (config, default `true`): whether other hubs may
  replicate from this one. Reads are public in v1 anyway, so `false` is a
  policy/load opt-out, not secrecy.
- The hubs this hub replicates from are its **peers** (hub peers —
  unrelated to exe's node peer sync). Like bans, peers live in SQLite, not
  config: admin-signed `peer.add` / `peer.remove` ops through `/v1/msg`,
  materialized into a derived `peers` table — live changes without a
  restart, with audit history. These ops are reserved, not implemented
  in v1.
- A peer entry is `{hub id, multiaddr}`: the multiaddr locates the hub,
  the id (its key fingerprint) authenticates it — on connect the remote
  hub must prove the ed25519 key matching the id, so a compromised DNS
  name or address can't impersonate a peer. v1 accepts only the HTTP
  multiaddr profiles — `/ip4|ip6|dns4|dns6/…/tcp/<port>/http|https`,
  optionally with `/http-path/…` for a hub behind a reverse-proxy prefix —
  and `peer.add` rejects anything else until a libp2p-style transport is
  actually wanted.
- **Config reload is manual, nginx-style.** Editing and saving
  `config.json` changes nothing by itself — no file watcher. The daemon
  re-reads config only on an explicit signal: `kill -HUP <pid>`, or the
  convenience `exe-hub -s reload`, which finds the running daemon via its
  pidfile and sends SIGHUP. On reload: re-parse, validate, atomic swap;
  an invalid config keeps the old one and logs the error. `admins`, gate
  settings, and `allow_replication` all take effect on reload.

## Envelope & operations

Envelope: `{type, author (pubkey b64), seq, ts, body}`.

- `profile.set` — create/update profile (display name, bio, avatar…).
  Last-write-wins by `seq`.
- `post.create` — text + up to 4 embeds. Post ID = the message's content
  hash (unforgeable reference). Optional `reply_to`: the post ID this post
  replies to. Content-hash IDs make the reference hub-independent, so
  replies survive aggregation; an unknown `reply_to` is stored as-is, not
  rejected (the parent may live on another hub or arrive later). Deleting
  a parent never cascades — replies stand on their own.
- `post.delete` — references a post ID. Always allowed for one's own posts,
  even when the author no longer passes the gate or is banned.
- `ban.set` / `ban.lift` — admin-only (author must be a config-assigned
  admin): ban or unban a target pubkey, with an optional reason on
  `ban.set`.
- No `post.edit` in v1.

Replay protection: per-author monotonic `seq`, enforced in SQLite — no
clock-skew windows. The client's claimed `ts` is kept for display only;
feed ordering uses hub receive time.

## Storage (SQLite, WAL mode)

- `messages` — append-only log of raw signed envelopes. **Source of truth.**
- `profiles`, `posts`, `embeds`, `pins`, `bans` — derived indexes,
  rebuildable by replaying `messages`. Schema changes to derived tables
  never lose data.
- Feed queries use keyset pagination (`before=<id>`), never OFFSET.

## Who may post — JSON config gate

```json
{
  "gate": {
    "mode": "token",
    "token": {
      "rpc_url": "https://mainnet.helius-rpc.com/?api-key=<key>",
      "mint": "9raUVuzeWUk53co63M4WXLWPWE4Xc6Lpn7RS9dnkpump",
      "min_amount": "10000000000",
      "recheck": "10m",
      "rpc_unavailable": "deny"
    }
  },
  "admins": ["<profile id (pubkey fingerprint)>"],
  "allow_replication": true
}
```

`config.json` is **gitignored** — the RPC URL embeds a Helius API key. A
committed `config.example.json` carries the shape with placeholders. The
launch mint is `9raU…pump` (6 decimals); the initial threshold is
**10,000 tokens**, i.e. `min_amount: "10000000000"`.

- `mode: "open"` (default) — any valid signature may post.
- `mode: "token"` — the author's Solana address must hold ≥ `min_amount`
  of `mint`. The address is **derived from the signing pubkey** (a Solana
  address is the raw 32-byte ed25519 pubkey in base58), so a signed post
  already proves control of the checked address — no wallet-linking flow.
  Holding requires no Solana signatures; the node never signs a
  transaction.
- Balance check: `getTokenAccountsByOwner(owner, {mint})`, summed, against
  both the classic SPL Token and Token-2022 program IDs. `min_amount` is
  raw base units as a **string** (u64s don't survive JSON floats).
- Verdicts cached per author for `recheck`; on RPC failure serve the cached
  verdict, and only with no cache at all does `rpc_unavailable` apply
  (default `deny`).
- Gate applies to `post.create` and `profile.set` at ingest only — never
  retroactive; a balance dropping doesn't vaporize history. `post.delete`
  bypasses the gate.
- **Moderation — bans live in SQLite, not config.** `admins` in the config
  names who may moderate; bans themselves arrive as signed `ban.set` /
  `ban.lift` messages through `/v1/msg` like every other mutation, land in
  the append-only log, and materialize in the derived `bans` table — so
  they apply without a restart, carry an auditable history, and survive a
  derived-table rebuild. Admin ops bypass the token gate; a `ban.set` from
  a non-admin is rejected at ingest.
- A ban blocks the target's `post.create` and `profile.set` at ingest.
  Like the token gate it is not retroactive: existing posts stay, and the
  banned author may still `post.delete` their own posts. (Admin deletion
  of others' posts is a separate power, deliberately not in v1.)
- Future escape hatch (not v1, but the envelope must not preclude it):
  `profile.set` may later carry a separate Solana address plus that
  address's signature over the author pubkey, so holdings can sit in a
  cold wallet while the node key only signs posts.

## Embeds & IPFS

- Up to 4 per post. Fields: `cid` (required, CIDv1), `mime` (required),
  `filename` (optional), `alt` (optional). Each ≤ 8 MB.
- **Hub-mediated upload only in v1:** client POSTs bytes → hub enforces
  size, sniffs the real MIME (doesn't trust the declaration), adds + pins
  via kubo RPC (`/api/v0/add`), returns the CID. No arbitrary external
  CIDs in v1 (fetching untrusted CIDs hangs and size-bombs; if ever
  allowed, hard timeout + size-capped reader before pinning).
- Pins are refcounted in SQLite; when a delete drops a CID's count to
  zero, unpin.
- Served through the hub: `GET /v1/embed/{cid}` proxies from the IPFS node
  with long cache headers — feed clients need no gateway or IPFS.

## HTTP API

- `POST /v1/msg` — all mutations (signed envelope).
- `POST /v1/upload` — embed bytes → CID.
- `GET  /v1/feed?before=<id>&limit=` — aggregated feed.
- `GET  /v1/profile/{id}` — profile info.
- `GET  /v1/profile/{id}/feed` — one author's feed.
- `GET  /v1/post/{id}` — one post plus its replies (keyset-paginated).
- `GET  /v1/embed/{cid}` — embed bytes proxy.

Reads are public; writes authenticate by signature alone.

## Client wiring (exe side)

- The browser Feed app never holds the key. The exe daemon grows a small
  `POST /v1/hub/publish`: the app submits an op, the daemon signs with the
  peer identity and forwards to the hub — same pattern as every other app
  (apps talk only to their own daemon).
- Reads can go straight from the Feed app to the hub.

## Open questions

- Replication mechanics between hubs (the trust model is settled: manual
  `peer.add` only): what replicates (posts only, or profiles too), how
  replicated content interacts with the local gate and bans, dedup and
  ordering of remote messages in the aggregated feed.
