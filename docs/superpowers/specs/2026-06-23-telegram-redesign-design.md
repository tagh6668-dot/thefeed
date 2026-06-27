# TheFeed — Telegram-2026 UI Redesign

Status: approved (mockups v3), implementation in progress.
Date: 2026-06-23.

## Goal

Re-present the existing app in the Telegram v12.4.0 (Feb 2026) visual language —
floating bottom navigation, chat-folder pills, a search bar that hides on scroll,
a slim header, and rounded translucent cards — **without losing or breaking a
single existing feature**. Both mobile and desktop must look good; Android +
iOS WebView parity and speed are hard requirements.

## Guiding principle: additive, zero-regression

The app has ~24 feature areas (channels, messages, media, scanner, resolver
bank, telemirror, messenger/chat, saved messages, profiles, settings, backup,
storage, i18n, …). Rather than rewrite the wiring, the redesign **overlays a new
navigation layer** that drives the existing, already-wired screens:

- The floating bottom nav (mobile) / left icon rail (desktop) replaces the
  sidebar toolbar buttons. Its tabs call the existing open functions.
- **Feed** tab = the existing sidebar (channel list) + chat-area, restyled.
- **Chat** tab = existing `openMessenger()`.
- **Scanner** tab = existing `openScanner()` / resolver bank.
- **Settings** tab = existing `openSettings()`, with Profiles surfaced on top.

Nothing is removed; everything is re-presented. This guarantees no feature loss.

## Navigation

Four tabs: **Feed · Chat · Scanner · Settings** (Mirror is a folder, not a tab).

- Mobile: a floating capsule pinned above the home indicator via
  `calc(12px + env(safe-area-inset-bottom))`; translucent blurred background;
  fully rounded corners; icon + label; active tab in accent; unread badges.
  Clean — no square pill behind the active item, no divider lines between tabs.
- Desktop: a 70px left icon rail (same destinations), then the resizable
  sidebar, then the conversation pane.
- **Hidden entirely when a channel or chat conversation is open** (full-screen
  reading), and when any modal/sheet is open. Restored on back.
- Hide-on-navigation-depth, NOT hide-on-scroll → sidesteps the Material
  accessibility caveat (never trap a screen-reader user).

## Feed tab

- **Slim header**: active-profile avatar + name (so the active profile is always
  visible) on the leading edge; overflow (⋮) on the trailing edge.
- **Full-width pill search** below the header; **hides on scroll down**, a search
  icon appears in the header; folder pills stay sticky. Kept visible while a
  screen reader is active.
- **Folder pills** (Telegram chat-folder style): `Feed`, `Mirror`, then channel
  filters (`Unread`, …). Active pill accent-filled with unread badge. Switching
  is instant and stays in the Feed tab.
- **Channel rows** become rounded translucent cards (no divider lines).

## Mirror folder (Telemirror integration)

Selecting the **Mirror** pill shows the telemirror channels in the feed list. A
dismissable note sits at the top, covering all of:

- Mirror loads channels over **Google** — no resolver needed.
- If **Google is unreachable/blocked**, Mirror won't load.
- Mirror supports **image download only** (no video/file downloads).
- You can **add your own channels**.
- The **3 default pinned channels** are always kept and cannot be removed.
- A "Don't show again" control hides the note (persisted).

## Cross-cutting requirements

- **i18n**: every new string added to both `fa` and `en` dictionaries; new DOM
  carries `data-i18n*`; RTL/LTR verified. `applyLang()` hardened so a missing
  element or a throwing renderer can't abort a language switch.
- **Speed / Android-WebView safety**: transform/opacity-only animations; no
  transitions on pane swaps; no `will-change` on images; no `backdrop-filter` on
  frequently-repainted/scrolling surfaces; no large blur box-shadows. Reuse the
  existing instant pane-swap to avoid the documented compositor hang.
- **Accessibility**: ARIA roles/labels on nav and folder tabs, `aria-current`,
  focus management, screen-reader-friendly hide rules.
- **Preserve subtle behaviors**: pull-to-refresh, refresh button, log panel,
  export, message search, floating date, scroll-to-bottom, media lightbox/
  download/queue, long-press → Saved, Android back handling, SSE live updates.

## Pre-work bug fixes (done)

1. **Language selection** — could not reproduce on a fresh build (works; likely a
   stale embedded-static binary). Hardened `applyLang()` defensively.
2. **Initial resolver check → early channel load** — added
   `ResolverChecker.SetOnFirstHealthy`; the web layer now starts loading channels
   the instant the first healthy resolver is found during the initial bank check,
   instead of waiting for the whole pass. Gated to when channels are still empty.

## Build / test

- Web UI binary: `go build -o build/client ./cmd/client` (gitignored path).
- Static JS/CSS are embedded via `//go:embed static` — rebuild to see changes.
- Test in Chrome (mobile-emulated + desktop widths); verify no console errors;
  verify each feature still reachable.
- READMEs (EN/FA/ZH/RU) updated if user-facing docs change; EN is source of truth.

## Out of scope (this pass)

Deep per-screen visual restyle of Scanner/Settings/Chat internals beyond what the
nav + card system gives for free — follow-up increments.
