# Dispatch console redesign

## Purpose

An operator supervises isolated coding agents, answers requests that only a human can finish, and controls the gateway that owns credentials and mediates network access. Monitoring and intervention are the primary loop; editing policy is secondary.

## Design plan

- Palette: graphite `#16191e`, raised graphite `#20252c`, chalk `#e4e8ef`, steel `#9faaba`, signal blue `#a5c6ff`, attention amber `#efc078`. Red and green are reserved for blocked/error and healthy/success states.
- Type: locally available humanist sans (Segoe UI / system fallback) for reading and controls; Consolas / monospace for addresses, commands and the signal strip. No remote fonts or assets on a credential-bearing console.
- Layout: left-aligned rail, broad work surface, quiet connection status at the foot. Mobile turns the rail into a wrapping navigation band, never a hidden hamburger.
- Signature: a live timeline with one fixed 100px row per project, stable alphabetical ordering, and sticky project labels. Fifteen minutes fit the available track width; three hours are available by horizontal scrolling, and scrolling back to the right edge resumes live following immediately. Recent semantic events have bottom-aligned 80%-height panels for 60 seconds; the newest stays at the leading edge and earlier panels slide left with no gaps, fully covering the blocks behind them. Panels are two-section: a 16px trace lane on top, the message body below. Thin lines in the trace lane connect each panel to its event block; the drawer runs on the one-second clock. Panels are passive; only the full-height event blocks respond to hover and focus, with details stacked above them. Activity renders full height like every other kind and never underlaps another kind; a different kind breaks its run. Activity hover/focus shows only the merged duration, such as "14 minutes active".
- Hierarchy: Projects / Tasks / Needs you / Traffic, followed by Access / Network / Policy. One attention inbox replaces four competing modal launchers. Incoming events update counts but never steal focus or approve anything.

```
Desktop                       Narrow
piso | Projects               piso   connection
     | intro + filters        Projects / Needs you / Traffic
nav  | project | timeline     Access / Network / Policy
     | project | timeline     Projects
     | project | timeline     project | timeline
     |         | now          project | timeline
sync |                        scroll history
```

## Review against the brief

A grid of status cards and giant counters would be a generic monitoring dashboard. Project-led timeline rows expose emerging work without decorative aggregate metrics. The leading edge takes priority over historical controls: no scale/range selectors, a Now button, and automatic return after 30 seconds without interaction. Nearby same-kind events merge within three minutes; full messages remain available by hover, keyboard focus, or tap. Activity & Context expansions are removed. Configuration is not forced into tables: name-led expandable records reserve full width for identifiers and regex. Forms use real labels and one-column mobile flow.

## Behavioral requirements

- Preview publication requires an explicit Publish and open action. It is not approval of the plan's contents. Popup denial must not silently publish a route.
- Real values are entered only in password fields, never populated from GET responses, never persisted in the browser.
- Existing API capabilities remain: asks, MCP authorization/revocation, GitHub app setup/install/remove, secret CRUD, provider configuration, rules/patterns/exceptions/domains, routes, worker egress controls, block resolution and replay.
- Pausing traffic pauses presentation only. Keep receiving bounded updates and label that distinction.
- SSE status is distinct from API health and Technocore pairing. Polls backstop stream events; partial failures are visible and retryable.
- The pairing line names the enrollment state in words (not connected / awaiting approval / approved · confirm / paired / error) and shows a sync age only once paired. An unpaired gateway has never synced, so it must never report an age; a missing timestamp is not "a very long time ago".
- Native dialogs supply focus trapping and Escape behavior. Error feedback stays alongside the action; no browser alerts. Destructive actions require confirmation.
- No runtime dependencies or frontend build step; the existing Go-embedded single HTML asset remains deployable.

## Task dashboard

Tasks sits directly below Projects. It uses compact linked rows rather than
summary cards, Active / Finished filters, server-side title search, and signed
cursor pagination. Details show authoritative task state, instructions,
acceptance criteria, checkpoint, result, artifacts, and updates. Waiting tasks
link to Needs you for decisions. The visible list/detail polls every ten seconds.

The native New task dialog uses the existing graphite/chalk/blue palette and
system typography. No worker selection: creation uses the shared Tasks queue.
Drafts remain in memory only. An ambiguous submission freezes the original
UUID and payload for retry; validation errors remain editable. Dismissal of a
nonempty draft requires confirmation. No new dependencies or build step.

## Live connection

One rail button carries the API/stream dot, the enrollment words, and a
tooltip with name, placement, fingerprint, and last error. It opens `#/live`
instead of linking out, so the whole enrollment lives on the console: the panel
shows *Show pairing code* (coordinator URL + name), then the QR with a
countdown, then **approved from a phone → Connect <gateway> to <account>?**, then
a read-only summary of the paired gateway. The terminal equivalent stays one
`<details>` away.

- The QR is a server-rendered SVG (`/api/v1/technocore/pair/qr.svg`), never a
  remote image service, and sits black-on-white on a light card so a dark
  console theme cannot invert it into an unscannable code. The pairing URL is
  also printed as a link, because a phone on the same network as the browser
  can simply open it.
- The panel polls every 1.5s **only** while `#/live` is open and the tab is
  visible; the ten-second global refresh is the backstop, and hidden tabs stop
  both. Re-rendering is gated on a state key so a poll cannot rebuild the panel
  under a finger; only the countdown text is patched each second.
- Nothing connects on the phone tap. The connect button is the only thing that
  mints the membership, and it names the account it will join. If the phone
  approved anonymously the API answers 409 and the panel swaps in a link to the
  signed-in coordinator page instead of a dead end.
- A paired gateway is read-only here: re-enrolling or switching coordinators is
  a terminal action, because it changes who can decrypt the vault.

## Validation and limits

- JavaScript syntax parsed successfully after formatting.
- The piso mark ships as an embedded `ui/favicon.svg`, served same-origin at `/favicon.svg` and `/favicon.ico` with the dashboard. No remote asset is fetched; the single embedded page still has no build step.
- `go test ./gateway/internal/server` passes with the replacement embedded UI.
- `git diff --check` passes.
- Browser verification was attempted but Chromium cannot launch in this worker because system libraries are missing. No system packages were installed. Responsive layouts, focus behavior, popups, and end-to-end mutations still need real-browser verification.
- The project timeline uses timestamps, not event-order strips. The existing activities endpoint supports `window=3h` to avoid truncation at the old 2,000-event cap. Project attention status reflects the latest reported semantic event in that window, not an authoritative task state. The topology diagram remains replaced by the inspectable traffic feed.
- Timeline tests: `node --test gateway/internal/server/ui/timeline.test.cjs` covers merging, window boundaries, callout layout/expiry, and return-to-now. A local jsdom smoke check also verifies page execution, merged hover details, escaped content, expiry, navigation, and follow behavior; it is not visual layout verification.
- Request presentation is bounded to 300 rows after live updates. There is no historical pagination. Pausing the feed freezes presentation, not collection.
- OAuth and preview publishing still depend on browser popup permissions; a blocked popup stops publication before the approval API is called.
