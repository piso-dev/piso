# Dispatch console redesign

## Purpose

An operator supervises isolated coding agents, answers requests that only a human can finish, and controls the gateway that owns credentials and mediates network access. Monitoring and intervention are the primary loop; editing policy is secondary.

## Design plan

- Palette: graphite `#16191e`, raised graphite `#20252c`, chalk `#e4e8ef`, steel `#9faaba`, signal blue `#a5c6ff`, attention amber `#efc078`. Red and green are reserved for blocked/error and healthy/success states.
- Type: locally available humanist sans (Segoe UI / system fallback) for reading and controls; Consolas / monospace for addresses, commands and the signal strip. No remote fonts or assets on a credential-bearing console.
- Layout: left-aligned rail, broad work surface, quiet connection status at the foot. Mobile turns the rail into a wrapping navigation band, never a hidden hamburger.
- Signature: a live timeline with one fixed 100px row per project, stable alphabetical ordering, and sticky project labels. Fifteen minutes fit the available track width; three hours are available by horizontal scrolling, and scrolling back to the right edge resumes live following immediately. Recent semantic events have bottom-aligned 80%-height panels for 60 seconds; the newest stays at the leading edge and earlier panels slide left with no gaps, fully covering the blocks behind them. Panels are two-section: a 16px trace lane on top, the message body below. Thin lines in the trace lane connect each panel to its event block; the drawer runs on the one-second clock. Panels are passive; only the full-height event blocks respond to hover and focus, with details stacked above them. Activity renders full height like every other kind and never underlaps another kind; a different kind breaks its run. Activity hover/focus shows only the merged duration, such as "14 minutes active".
- Hierarchy: Projects / Needs you / Traffic, followed by Access / Network / Policy. One attention inbox replaces four competing modal launchers. Incoming events update counts but never steal focus or approve anything.

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
- Native dialogs supply focus trapping and Escape behavior. Error feedback stays alongside the action; no browser alerts. Destructive actions require confirmation.
- No runtime dependencies or frontend build step; the existing Go-embedded single HTML asset remains deployable.

## Validation and limits

- JavaScript syntax parsed successfully after formatting.
- `go test ./gateway/internal/server` passes with the replacement embedded UI.
- `git diff --check` passes.
- Browser verification was attempted but Chromium cannot launch in this worker because system libraries are missing. No system packages were installed. Responsive layouts, focus behavior, popups, and end-to-end mutations still need real-browser verification.
- The project timeline uses timestamps, not event-order strips. The existing activities endpoint supports `window=3h` to avoid truncation at the old 2,000-event cap. Project attention status reflects the latest reported semantic event in that window, not an authoritative task state. The topology diagram remains replaced by the inspectable traffic feed.
- Timeline tests: `node --test gateway/internal/server/ui/timeline.test.cjs` covers merging, window boundaries, callout layout/expiry, and return-to-now. A local jsdom smoke check also verifies page execution, merged hover details, escaped content, expiry, navigation, and follow behavior; it is not visual layout verification.
- Request presentation is bounded to 300 rows after live updates. There is no historical pagination. Pausing the feed freezes presentation, not collection.
- OAuth and preview publishing still depend on browser popup permissions; a blocked popup stops publication before the approval API is called.
