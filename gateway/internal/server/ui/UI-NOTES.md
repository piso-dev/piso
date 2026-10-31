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
- Coordinator-owned views (Tasks, Directives) are gated on an active pairing, not on a failed request. The page renders one info banner naming the enrollment state and turns its create actions off before asking the API; a `503 technocore_unpaired` refusal is a state, never a refresh error. The nav keeps those destinations reachable and only mutes them, because the banner that explains them lives on the page. That banner links into the same `#/live` panel the rail button opens when the request is waiting for approval.
- The pairing line names the enrollment state in words (not connected / awaiting approval / approved · confirm / paired / error) and shows a sync age only once paired. An unpaired gateway has never synced, so it must never report an age; a missing timestamp is not "a very long time ago". The gated nav links describe themselves with that same line, via `aria-describedby="live"`.
- Native dialogs supply focus trapping and Escape behavior. Error feedback stays alongside the action; no browser alerts. Destructive actions require confirmation.
- No runtime dependencies or frontend build step; the existing Go-embedded single HTML asset remains deployable.

## Task dashboard

Tasks sits directly below Projects. It uses compact linked rows rather than
summary cards, Active / Finished filters, server-side title search, and signed
cursor pagination. Details show authoritative task state, instructions,
acceptance criteria, checkpoint, result, artifacts, and updates. Waiting tasks
link to Needs you for decisions. The visible list/detail polls every ten seconds,
and a poll re-renders in place: the window scroll and the board's own sideways
scroll are read before the rebuild and put back after it, so a refresh never
throws the reader to the top left or back to the first column.

The native New task dialog uses the existing graphite/chalk/blue palette and
system typography. No worker selection: creation uses the shared Tasks queue.
Drafts remain in memory only. An ambiguous submission freezes the original
UUID and payload for retry; validation errors remain editable. Dismissal of a
nonempty draft requires confirmation. No new dependencies or build step.

Approving a proposal reuses that same form rather than a second editor: the
Title, Instructions and Acceptance criteria fields arrive filled with the
proposal, under the same limits, so the human sees exactly what will be queued
and edits it in the place it was written. One extra field at the bottom carries
further instructions for the worker; it is optional, and only fields the human
actually rewrote travel as an amendment. An approval that carries no task text
keeps the single decision note.

A newly created task is held for a grace period, and the hold is drawn as one
panel rather than a badge plus a loose verb: a green-bordered, green-tinted
block with white text, the countdown at its top left and "Make available now"
at its bottom right. State and action in one object means they cannot drift into
different corners of the card. Green is the panel's colour because the hold
expires by itself, so it is not a warning, and the panel turns blue with
"Held open by …" when somebody reading the task is what is holding it,
because then nothing is counting down. The same panel renders on the board card
and on the detail page; the timer patches only the clock's text node once a
second so a tick cannot steal focus or scroll position.

The board colours what a card *is*, not how far along it is: state is already
said twice, by the column and by the status badge, so the hue is spent on type
instead. Each card is washed in its type's colour — a solid, low-key tint
(16% of the hue mixed into the graphite panel, so the card stays a card on the
neutral column rather than going translucent) — and carries, above the status,
a tinted pill naming that type in words. There is no left colour block: a 3px
rule down one edge read as a legend you had to learn, while a tint names the
whole object. Blue is plain work; violet is
a proposal or a decision (approve or reject); teal is a review (accept or send
back); amber is a question or a human action, both of which wait on the same
thing, someone answering; moss is a preview to publish; and grey is an access
grant, the one request that is a credential rather than a piece of work. Plain
work carries no chip: it is the default, and its blue tint is the only label it
needs. The hue is never the only cue, and two types share a hue only when the
operator owes them the same verb, so the chip still names the exact type.
Columns stay neutral, so the board has one colour legend rather than two.
Red and green stay out of that legend: they already mean failure and health.
The detail page repeats the same hue as a left rule plus the same chip in its
header, so a card and its detail cannot disagree about what the thing is. The grace panel keeps its
stronger solid green border, so a hold still reads as an object inside a card.

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
- The phone page signs in first: `/pair/<id>/` sends a stranger through
  `/login/?next=/pair/<id>/` and returns them to the same request, so the
  approval always names the person who gave it. Login is also what lets the
  phone finish the enrollment itself.
- Nothing connects on the phone tap. The connect button is the only thing that
  mints the membership, and it names the account it will join. A request
  approved without a session (an older client writing to the API) still cannot
  be finished here: the API answers 409 and the panel swaps in a link to the
  signed-in coordinator page instead of a dead end.
- A paired gateway is read-only here: re-enrolling or switching coordinators is
  a terminal action, because it changes who can decrypt the vault.

## Validation and limits

- JavaScript syntax parsed successfully after formatting.
- The piso mark ships as an embedded `ui/favicon.svg`, served same-origin at `/favicon.svg` and `/favicon.ico` with the dashboard. No remote asset is fetched; the single embedded page still has no build step.
- `go test ./gateway/internal/server` passes with the replacement embedded UI.
- Board type test: `node --test gateway/internal/server/ui/board-types.test.cjs`
  checks that colour follows the item type rather than the status (five states
  of plain work share one rule and no chip), and that every request type gets
  its chip from the ask it carries.
- `git diff --check` passes.
- Browser verification runs in a real Chromium from `.tools/pw` (see
  `.tools/board-visual.sh` and `.tools/grace-visual.py`): geometry, computed
  colours and spacing are measured in the browser, not inferred from the CSS.
- The project timeline uses timestamps, not event-order strips. The existing activities endpoint supports `window=3h` to avoid truncation at the old 2,000-event cap. Project attention status reflects the latest reported semantic event in that window, not an authoritative task state. The topology diagram remains replaced by the inspectable traffic feed.
- Timeline tests: `node --test gateway/internal/server/ui/timeline.test.cjs` covers merging, window boundaries, callout layout/expiry, and return-to-now. A local jsdom smoke check also verifies page execution, merged hover details, escaped content, expiry, navigation, and follow behavior; it is not visual layout verification.
- Request presentation is bounded to 300 rows after live updates. There is no historical pagination. Pausing the feed freezes presentation, not collection.
- OAuth and preview publishing still depend on browser popup permissions; a blocked popup stops publication before the approval API is called.
