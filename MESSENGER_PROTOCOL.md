# thefeed Messenger Protocol — Draft RFC

- **Status:** DRAFT / pre-release. Request for comments.
- **Wire version:** 1 (sealed-cell transport); message envelope v1; registration record v1.
- **Stability:** No backward-compatibility guarantees yet. The messenger has not
  shipped a stable release, so breaking changes are on the table — that is the
  point of this document. Please tear it apart.

This document describes the end-to-end encrypted "lite messenger" that
thefeed tunnels over DNS. It is written so people who have never read the code
can review the design. Wire details are given to the byte; where this document
and the code disagree, the code is the source of truth — please file that as a
bug against this document.

Domains shown as `chat.example.com` are placeholders.

---

## 1. Goals and non-goals

**Goals**

1. **Works where only DNS works.** Every operation is a DNS query/response on a
   small set of operator sub-domains. No TCP, no HTTP, no TLS SNI.
2. **End-to-end confidentiality.** The server stores and forwards ciphertext it
   cannot read. Only the two endpoints derive the message content key.
3. **Indistinguishable queries.** A passive resolver sees fixed-length,
   random-looking DNS labels. It cannot tell a registration from a send from a
   poll, nor recover the operation, the peer, or the read metadata.
4. **Fail-closed.** A client refuses to chat with a server whose feed-signing
   key it has not pinned. There is no opportunistic / unauthenticated mode.
5. **Amnesia-safe client.** A client that loses all local state can recover its
   identity from a 32-byte seed and resynchronise sequence numbers from the
   server.

**Non-goals (today)**

- Hiding *metadata from the server*: the server necessarily learns the routing
  pair (sender address → recipient address), message sizes, and timing. See §13.
- Forward secrecy of message content (the pair content key is derived from
  long-term identity keys; see Open Questions §15).
- Group chat, multi-device sync, voice/media. One-to-one text only.
- Hiding *that DNS is being used* from a network observer who can see the
  destination domain.

---

## 2. Threat model

| Adversary | Assumed power | What the protocol defends |
|-----------|---------------|---------------------------|
| Passive resolver / on-path DNS | Sees every query name + response, knows the *public* feed passphrase | Cannot recover op, peer, counters, or content; sees uniform random labels |
| Malicious/compromised server | Stores & routes all ciphertext, can drop/delay/reorder, can lie | Cannot read content; cannot substitute a peer's key (address = hash of identity key); cannot forge a delivery; cannot impersonate (per-op seal + pinned signing key) |
| Active forger on the wire | Can inject DNS answers | Sealed responses fail closed; a forged 4-byte op tag costs ~2³² rate-limited online round-trips and only yields E2E ciphertext |
| Peer | A registered account you talk to | Standard E2E peer; cannot misattribute a message to a third party (content key binds src,dst,seq) |

Explicit residual exposure to the **server**: routing pair, sizes, timing,
social graph among accounts on that server. This is the cost of store-and-forward
over a third-party relay; mitigations are an open question (§15).

---

## 3. Terminology

- **Identity key** — ed25519 key pair derived from the user's seed. Names the user.
- **Address** — first 12 bytes of `SHA-256(identity_pub)`. 20 base32 chars when shared.
- **Encryption key (enc key)** — x25519 key pair derived from the seed. Used for E2E and for the client↔server shared key.
- **ek** — the *server's* x25519 key. Clients run the session handshake against it.
- **Registration record** — self-signed envelope binding `enc_pub` to an identity, published to a server.
- **ChatInfo** — the server's signed capability descriptor (limits, domains, `ek`).
- **Cell** — the single uniform 25-byte unit every chat query carries.
- **Session** — a per-connection symmetric key (`Ksession`) plus a server-assigned 3-byte selector.
- **Pair** — an ordered (sender, recipient) relationship; sequence numbers and delivery counters are per-pair-per-server.

---

## 4. Architecture overview

```
   client  ───DNS queries on chat.example.com───▶  server (relay + key store)
     │  E2E content key (server never has it)            │
     └───────────────── message ciphertext ─────────────┘
                         (store-and-forward)
```

- Chat rides the same DNS transport as the feed reader, but on dedicated chat
  sub-domains. The server dispatches by domain; chat names are never parsed as
  feed channel queries.
- **Trust root:** the client pins the server's **feed-signing public key** (ed25519,
  carried in the `thefeed://…?sk=` import URI). The `ChatInfo` descriptor —
  including the server's `ek` — is served on a metadata channel and authenticated
  by a signature (an "ExtraBlock") under that pinned key. So the key the client
  runs ECDH against (`ek`) is delivered in-band but **signed**; a MITM cannot
  swap it.
- The server keeps, per account: a bounded inbox of ciphertext envelopes, the
  account's published registration record, and per-pair sequence/quota state.
  Everything is TTL'd.

### 4.1 Multiple chat domains and one identity

A server may advertise several chat sub-domains in `ChatInfo.domains[]`. The
client spreads queries across them (rotating by cell index + retry attempt) for
resilience against per-domain blocking or rate limiting; every domain reaches the
same server and the same account state.

Because identity is a single key pair, the user has the **same address on every
server**. A conversation binds to the one server it was started on (sequence
numbers and delivery ticks are per-server, §11.3/§12). Switching a conversation's
send server is an explicit user action that first re-checks the peer is
registered there; a deliberate switch is never silently reverted by an in-flight
send.

---

## 5. Identity and addresses

```
seed (32 bytes, the recovery secret)
 ├─ HKDF-SHA256(info="thefeed-chat-identity-v1")   → ed25519 identity key
 └─ HKDF-SHA256(info="thefeed-chat-encryption-v1") → x25519  enc key

address = SHA-256(identity_pub)[:12]               (12 bytes → 20 base32 chars)
```

The address is a hash commitment to the identity key, so a server cannot present
a different identity/enc key for a given address without being detected (the
client recomputes `address == SHA-256(identity_pub)[:12]` on every key fetch).

---

## 6. Registration (publishing your key)

A client publishes a **registration record** so peers can fetch its `enc_pub`
and so the server can authenticate it.

```
RegisterEnvelope (133 bytes):
  ver(1)=1 ‖ identity_pub(32) ‖ enc_pub(32) ‖ timestamp(4, BE unix) ‖ ed25519_sig(64)
  sig = Sign(identity, "thefeed-chat-register-v1" ‖ body_before_sig)
```

- Self-signed by the identity key; newest timestamp wins on re-registration.
- A peer fetching this record verifies the signature **and** that
  `Address(identity_pub)` equals the address it asked for.
- Registration happens via the REGISTER handshake (§9), which both opens a
  session and stores the record.
- Registration is **per server** and **opt-in**: the client only publishes to
  servers the user explicitly enabled. An untrusted server never silently
  receives the user's identity.

---

## 7. Capability discovery — `ChatInfo`

Served on a dedicated metadata channel, block-split like feed titles, with a
trailing signature block verified against the pinned signing key. TLV-encoded
(unknown types skipped, so fields can be added):

```
ChatInfo:
  min_version, max_version
  enabled            (operator can configure chat domains but turn chat off)
  domains[]          (the chat sub-domains, for multi-domain spreading)
  ek_pub(32)         (server x25519 session key)
  limits             (see §14)
```

A client that cannot fetch a valid, signed `ChatInfo` does not chat (fail-closed).

---

## 8. Transport — the uniform cell

Every chat query is exactly one **cell**:

```
cell (25 bytes) = selector(3) ‖ counter(3) ‖ payload(19)
```

encoded as a single base32 DNS label (40 chars) under a chat domain, e.g.
`<40 chars>.chat.example.com`. (A "double" multi-label hex encoding exists for
resolvers that mangle long single labels.)

**Masking.** `selector ‖ counter` are XOR-masked with a keystream
`HMAC-SHA256(query_key, "thefeed-chat-cell-mask-v1" ‖ payload)[:6]`. The payload varies per cell (it is sealed
ciphertext or a random-looking ephemeral-key slab), so the mask — and therefore
the visible label — is different every time, with no constant per-session prefix
and no low-counter zero runs. A resolver without the public passphrase (which
gates `query_key`) sees uniform randomness; the server, holding `query_key` and
the payload at a fixed offset, recovers `selector` and `counter`.

The cell has **two byte-indistinguishable uses**:

- **In-context** (an operation on a live session): `selector` = the server's
  3-byte session ref; `payload` = `SealChat(Ksession, selector, counter, op‖fields)`.
- **Handshake**: `selector` = a client-chosen *setup tag* (top bit set to mark
  it); `payload` = a 19-byte slab of the handshake stream.

Dispatch: the server looks `selector` up in its live-session table. Hit →
in-context. Miss → if the handshake bit is set (or `counter == 0`), treat as a
handshake cell; otherwise return the session-lost sentinel (§10).

### 8.1 The per-op seal

```
SealChat(Ksession, selector, counter, pt):
  enc = HKDF(Ksession, info="thefeed-chat-seal-enc-v1")
  mac = HKDF(Ksession, info="thefeed-chat-seal-mac-v1")
  nonce = selector(≤4) ‖ counter(4)            (unique per sealed query)
  ct  = AES-256-CTR(enc, nonce, pt)
  tag = HMAC-SHA256(mac, nonce ‖ ct)[:4]
  return ct ‖ tag
```

Plaintext is zero-padded to a fixed **15 bytes** (`op(1) + fields(14)`) so every
in-context cell is identical length. The 4-byte tag is deliberately short:
forging it requires ~2³² *online*, rate-limited DNS round-trips, and success only
yields an E2E-encrypted payload. (Reviewers: is 4 bytes the right trade? §15.)

---

## 9. Session handshake

A session is a fresh ephemeral x25519 key on the client side, ECDH'd against the
server's `ek`:

```
Ksession = HKDF( ECDH(eph_priv, ek_pub),
                 info = "thefeed-chat-session-v1" ‖ query_key )
```

Mixing `query_key` into the HKDF info keeps the public passphrase a hard
requirement even though `ek` is published.

The client streams a **handshake stream** across one or more handshake cells
(counter = chunk index), which the server reassembles:

```
stream = eph_pub(32) ‖ kind(1) ‖ sealed_bootstrap
sealed_bootstrap = SealChat(Ksession, setup_tag, BOOTSTRAP_COUNTER=0x400000, bootstrap_plaintext)
```

`eph_pub` is cleartext (random-looking); the bootstrap is sealed.

Two kinds:

- **AUTH** (`kind=1`) — prove control of an already-registered account:
  ```
  bootstrap = addr(12) ‖ ts(4) ‖ proof(8)
  proof = HMAC( kss, "thefeed-chat-acct-proof-v1" ‖ eph_pub ‖ addr ‖ ts ‖ domain )[:8]
  kss   = HKDF( ECDH(enc_priv, ek_pub),
                info = "thefeed-chat-server-v1" ‖ client_enc_pub ‖ ek_pub )
  ```
  Only the account holder can compute `kss`, hence `proof`. The **domain** is
  bound into the proof, so a proof captured on one server cannot be replayed on
  another. `ts` is checked against server time within a skew window; a
  clock-skewed client is told the server's time and retries once.

- **REGISTER** (`kind=2`) — first contact: `bootstrap = RegisterEnvelope` (§6).
  The server verifies the signature, stores the record, and opens the session.

On success the server allocates a random 3-byte **session ref** (handshake bit
cleared), returns it sealed under `Ksession` (with the server's unix time and the
session TTL), and the client uses it as the `selector` for all subsequent
in-context cells.

`ensureSession` tries AUTH first; on `unknown_sender` it falls back to REGISTER.

---

## 10. In-context operations

Op byte = `version<<4 | op` in the first sealed plaintext byte.

| Op | # | Plaintext fields | Purpose |
|----|---|------------------|---------|
| `INBOX_STATUS` | 1 | — | list waiting messages + quota |
| `INBOX_FETCH`  | 2 | `seq(4) ‖ block(1)` | fetch one envelope block |
| `ACK`          | 3 | `peer_handle(4) ‖ up_to_seq(3)` | confirm delivery, free inbox |
| `KEY_FETCH`    | 4 | `addr(12)` | fetch a peer's registration record |
| `SEND_STATUS`  | 5 | `addr(12)` | read ✓/✓✓ counters for messages I sent |
| `SEND_START`   | 6 | `dst(12) ‖ total_len(2)` | begin an upload |
| `DATA`         | 7 | `idx(1) ‖ chunk(≤13)` | one body chunk |
| `FIN`          | 8 | `crc32(4)` | commit the upload |

Status codes (in the sealed response, first byte):

```
0  OK              1  unknown_recipient   2  inbox_full       3  pair_quota
4  rate_limited    5  unknown_sender      6  bad_version      7  busy
8  unknown_session 9  bad_auth           11  not_found       12  incomplete
13 replay          14 bad_request        15  disabled
```

**Responses** are sealed under `Ksession` with a response-side counter
(`req_counter | 0x800000`, kept disjoint from request nonces) and then wrapped by
the feed's normal passphrase response layer inside a TXT record. If the server no
longer knows the session (TTL expiry or reboot) it cannot produce a sealed reply,
so it returns a 1-byte **session-lost sentinel** (`0xE5`); the client
transparently re-handshakes and retries.

Counter discipline: in-context counters live below the reserved regions
(`0x400000` bootstrap, `0x800000` response bit). A client re-handshakes well
before exhausting the 22-bit space; the server rejects any in-context counter
≥ `0x400000`.

---

## 11. Sending a message

### 11.1 Content encryption (end to end)

```
content_key = HKDF( ECDH(sender_enc_priv, recipient_enc_pub),
                    info = "thefeed-chat-content-v1" ‖ src ‖ dst ‖ seq )
```

Unique per `(src, dst, seq)`, so the inner body is sealed with **AES-256-GCM
under a fixed zero nonce** — no nonce on the wire, and never reused because the
key itself never repeats.

```
inner   = cflag(1) ‖ (deflate(text) | raw text)   ; cflag picks the smaller
envelope = ver(1)=1 ‖ seq(4) ‖ GCM_ciphertext ‖ srv_mac(8)
srv_mac  = HMAC( kss, "thefeed-chat-srvmac-v1" ‖ src ‖ dst ‖ seq ‖ SHA-256(ct) )[:8]
```

- The **sender address is not in the envelope.** The recipient derives
  `content_key` using the `src` from its inbox entry; a wrong `src` yields a wrong
  key and GCM fails. Misattribution is therefore impossible.
- `srv_mac` authenticates the envelope *to the server* (only the registered
  holder of `sender_enc_pub` can compute `kss`), binding the routing pair, the
  seq, and the exact ciphertext. The server checks it at `FIN`.

### 11.2 Chunked upload (selective-repeat)

The envelope is split into ≤13-byte chunks (one chunk min, even for empty):

```
SEND_START(dst, total_len)
   → server allocates a reassembler for this session; replies a bitmap of
     chunks it already has (empty on a fresh start).
DATA(idx, chunk)  ×N
   → server fills the reassembler; each reply carries the updated bitmap, so the
     client only resends missing chunks (DNS is lossy).
FIN(crc32)
   → server checks  len == total_len  &&  crc32(assembled) == crc32,
     parses the envelope, verifies srv_mac, then CommitMessage(src, dst, seq).
```

Integrity in depth: every cell is individually sealed (4-byte tag), the whole
body is CRC32-checked at FIN, and the envelope carries the 8-byte server MAC and
the GCM tag. The CRC is for accidental corruption; the MAC/GCM are the
cryptographic guarantees.

### 11.3 Sequence numbers and idempotency

Sequence is **per (pair, server)**, recoverable from the server (`SEND_STATUS`
returns the last accepted seq, so an amnesiac client resyncs). Commit is
idempotent by seq:

```
seq <  last_accepted  → replay        (rejected; client bumps to last_accepted+1)
seq == last_accepted  → OK            (idempotent re-commit; safe to retry)
seq == last_accepted+1 → accept
```

This makes the whole send retry-safe: a lost `FIN`-OK, a re-handshake mid-upload,
or a duplicate all converge to "delivered exactly once". On any transport error
the client reconciles via `SEND_STATUS` before re-sending.

---

## 12. Receiving and delivery state

```
INBOX_STATUS → quota ‖ [ (src(12), seq(4), len(2), blocks(1)) … ]
INBOX_FETCH(seq, block) → one block of the envelope
ACK(peer_handle, up_to_seq) → frees src→me messages ≤ up_to_seq, bumps last_delivered
```

`peer_handle` is the first 4 bytes of the peer address; the server disambiguates
it within the caller's own known pairs (a collision only ever names another of
*your* contacts).

**Delivery ticks** are per-pair-per-server counters the sender reads with
`SEND_STATUS`:

- `last_accepted` (✓) — the server stored the message.
- `last_delivered` (✓✓) — the recipient fetched **and** ACKed it (the ACK both
  frees the server copy and raises this counter).

Because seq is per-server, these counters are **per-server** too: a message sent
on server A keeps its ✓✓ even after the conversation switches to server B.

---

## 13. Server state and limits

Per account the server holds: the published registration record, a bounded
inbox of ciphertext envelopes, per-pair `(last_accepted, last_delivered)`, and
send-rate counters. Sessions live in RAM only.

Default limits (advertised in `ChatInfo`, operator-tunable):

| Limit | Default | Meaning |
|-------|---------|---------|
| chunk size | 13 | body bytes per DATA cell (advertised so clients size uploads) |
| max message bytes | 500 | per message plaintext |
| inbox cap | 50 | undelivered messages per account |
| per-pair cap | 10 | undelivered messages per (sender→me) pair |
| sends/hour | 30 | per account |
| session idle | 120 s | re-handshake after idle |
| session hard | 600 s | absolute session lifetime |
| message TTL | 72 h | undelivered messages expire |

**What the server learns:** routing pairs (who messages whom on this server),
message sizes and counts, timing, and the social graph of accounts registered on
it. **What it cannot learn:** message content. Reducing the metadata exposure is
an open problem (§15).

---

## 14. Cryptographic constructions (summary)

| Purpose | Construction | Key / info |
|---------|--------------|------------|
| Identity | ed25519 | HKDF(seed, "…identity-v1") |
| Enc key | x25519 | HKDF(seed, "…encryption-v1") |
| Address | SHA-256(id_pub)[:12] | — |
| Session key | HKDF(ECDH(eph, ek)) | info ‖ query_key, "…session-v1" |
| Per-op seal | AES-256-CTR + HMAC-SHA256[:4] | enc/mac sub-keys of Ksession |
| Client↔server (kss) | HKDF(ECDH(enc, ek)) | "…server-v1" ‖ client_enc ‖ ek |
| Account proof | HMAC(kss)[:8] | "…acct-proof-v1" ‖ eph ‖ addr ‖ ts ‖ domain |
| E2E content | HKDF(ECDH(enc,enc)) | "…content-v1" ‖ src ‖ dst ‖ seq |
| Message body | AES-256-GCM, fixed zero nonce | content_key (unique per src,dst,seq) |
| Server MAC | HMAC(kss)[:8] | src ‖ dst ‖ seq ‖ SHA-256(ct) |
| Registration | ed25519 signature | "…register-v1" |

HKDF is HKDF-SHA256 throughout. All multi-byte integers are big-endian.

---

## 15. Open questions for reviewers

These are the design choices we are least sure about. Comments very welcome.

1. **4-byte per-op seal tag.** Adequate given online rate-limiting and that a
   forgery only yields E2E ciphertext — or false economy? Cell space is tight
   (15 plaintext bytes); a longer tag costs payload.
2. **No forward secrecy for content.** `content_key` derives from long-term enc
   keys, so a seed compromise decrypts all past messages held by the server.
   Worth a ratchet (e.g. X3DH + Double Ratchet) given the constraints, or out of
   scope for a "lite" relay messenger?
3. **Fixed zero GCM nonce.** Safe *iff* `content_key` is truly never reused.
   Reuse would happen only on a seq collision within a pair; the per-pair seq is
   server-anchored and idempotent. Is there a reachable reuse path we missed?
4. **Metadata to the server.** The relay sees the full routing graph. Sealed
   sender addresses, cover traffic, or per-recipient pseudonymous mailboxes are
   all possible — what is the right cost/benefit here?
5. **`peer_handle` = 4 bytes** disambiguated within the caller's own pairs for
   ACK/SEND_STATUS. Acceptable, or should these carry the full address?
6. **Account proof replay window.** Domain-bound and timestamped; is the skew
   handling (one clock-corrected retry) safe against a replay-within-window?
7. **DoS / abuse.** Rate limits are per account, but registration is cheap
   (self-signed). Should registration carry a proof-of-work or be invite-gated?
8. **Versioning / negotiation.** `ChatInfo` advertises min/max version and the op
   byte reserves a version nibble, but there is no per-op negotiation yet. Enough
   for a v1→v2 migration?

---

## 16. Security considerations (collected)

- **Trust anchoring:** all server-provided material (`ek`, limits, domains) is
  authenticated under the pinned feed-signing key. Pin distribution (the
  `thefeed://…?sk=` URI) is out of band and the weakest link — a bad pin is a bad
  everything.
- **Key substitution:** defeated by `address = hash(identity_pub)` plus the
  recipient re-checking it on every key fetch.
- **Cross-server replay:** the account proof binds the chat domain.
- **Fail-closed everywhere:** unverifiable `ChatInfo`, an unknown session, or a
  bad seal all stop the operation rather than degrading.
- **Compression:** inbound bodies are size-capped before inflation to bound a
  decompression-bomb to memory.

---

## 17. How to comment

Open an issue or PR against this file. For wire-level proposals, please reference
the section and give the concrete byte layout. Because the protocol is
pre-release, **incompatible** improvements are in scope — propose freely.
