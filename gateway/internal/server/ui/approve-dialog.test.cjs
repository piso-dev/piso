// Requires jsdom. Run with NODE_PATH pointing to its node_modules directory:
// NODE_PATH=.tools/ui-check/node_modules node --test gateway/internal/server/ui/approve-dialog.test.cjs
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const { JSDOM } = require("jsdom");

const html = fs.readFileSync(`${__dirname}/index.html`, "utf8");
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const tick = () => new Promise((resolve) => setTimeout(resolve, 20));

const PROPOSAL = {
  id: "prop-1",
  title: "Add a host",
  body: "Put piso.local on the ingress.",
  acceptanceCriteria: "The route answers.",
};

const ask = (extra) => ({
  kind: "approval",
  id: "a-1",
  from: "ship",
  title: "Add a host",
  action: "task.propose",
  taskId: PROPOSAL.id,
  revision: "R2",
  ...extra,
});

async function harness(t, asks, task = PROPOSAL) {
  const dom = new JSDOM(html.replace(/<script>[\s\S]*<\/script>/, ""), {
    url: "http://piso.local/#/tasks",
    runScripts: "outside-only",
    pretendToBeVisual: true,
  });
  t.after(() => dom.window.close());
  const w = dom.window;
  const calls = [];
  w.scrollTo = () => {};
  w.EventSource = class {
    addEventListener() {}
  };
  w.HTMLDialogElement.prototype.showModal = function () {
    this.open = true;
  };
  w.HTMLDialogElement.prototype.close = function () {
    this.open = false;
  };
  w.AbortSignal.timeout = () => new w.AbortController().signal;
  w.setInterval = () => 0;
  w.fetch = async (path, opts = {}) => {
    const body = opts.body && JSON.parse(opts.body);
    calls.push({ path, method: opts.method || "GET", body });
    let result = [];
    if (path.startsWith("/api/v1/tasks") && path !== "/api/v1/tasks") result = task;
    else if (path.startsWith("/api/v1/tasks")) result = { tasks: [], nextCursor: null };
    else if (path.endsWith("/asks")) result = { asks };
    else if (path.endsWith("/technocore")) result = { status: "active" };
    else if (path.endsWith("/health")) result = {};
    return { ok: true, status: 200, json: async () => result };
  };
  w.eval(script + "\nwindow.t = {decideAsk, data, loadTasks};");
  await tick();
  await w.t.loadTasks();
  return { w, doc: w.document, calls };
}

const submit = (w, doc) =>
  doc
    .querySelector("#actionForm")
    .dispatchEvent(new w.Event("submit", { bubbles: true, cancelable: true }));
const labels = (doc) =>
  [...doc.querySelectorAll("#actionForm label.field span")].map((s) => s.textContent);
const inputs = (doc) =>
  Object.fromEntries(
    [...doc.querySelectorAll("#actionForm [name]")].map((el) => [el.name, el]),
  );

// Approving a proposal is the New task modal with the proposal already in it:
// same fields, same order, same limits, and one extra field at the bottom for
// what the worker should hear on top of the work itself.
test("approving a proposal shows the New task fields and a note at the bottom", async (t) => {
  const { w, doc } = await harness(t, [ask()]);
  await w.t.decideAsk("approved", "a-1");
  await tick();
  assert.deepEqual(labels(doc), [
    "Title",
    "Instructions",
    "Acceptance criteria",
    "Further instructions for the worker (optional)",
  ]);
  const field = inputs(doc);
  assert.equal(field.title.value, PROPOSAL.title);
  assert.equal(field.body.value, PROPOSAL.body);
  assert.equal(field.acceptanceCriteria.value, PROPOSAL.acceptanceCriteria);
  assert.equal(field.summary.value, "");
  assert.equal(field.summary.required, false, "further instructions stay optional");
  assert.equal(field.title.required, true);
  assert.equal(field.title.maxLength, 300);
  assert.equal(field.body.maxLength, 20000);
  assert.equal(field.body.rows, 6);
});

test("only rewritten fields travel with the decision", async (t) => {
  const { w, doc, calls } = await harness(t, [ask()]);
  await w.t.decideAsk("approved", "a-1");
  await tick();
  const field = inputs(doc);
  field.acceptanceCriteria.value = "The route answers and stays up.";
  field.summary.value = "Keep the docs updated.";
  submit(w, doc);
  await tick();
  const post = calls.filter((c) => c.method === "POST");
  assert.equal(post.length, 1);
  assert.equal(post[0].path, "/api/v1/asks/approvals/a-1/decide");
  assert.deepEqual(post[0].body, {
    decision: "approved",
    summary: "Keep the docs updated.",
    seenRevision: "R2",
    amend: { acceptanceCriteria: "The route answers and stays up." },
  });
});

// An approval that carries no task text still has nothing to amend, so it keeps
// the single decision note.
test("other approvals keep the single decision note", async (t) => {
  const { w, doc } = await harness(t, [
    ask({ action: "publish", taskId: undefined, revision: undefined }),
  ]);
  await w.t.decideAsk("approved", "a-1");
  await tick();
  assert.deepEqual(labels(doc), ["Instructions for the worker (optional)"]);
});

// A host request is resolved by reporting what the command printed, so the one
// field asks for output rather than guidance, and the prompt repeats the command
// with the rendezvous path the worker will read.
test("resolving a host request asks what the command printed", async (t) => {
  const { w, doc } = await harness(t, [
    {
      kind: "human",
      id: "h-1",
      category: "host",
      title: "Host: docker build",
      instructions: "docker build -t piso .",
      verification: "/workspace/build.log",
    },
  ]);
  await w.t.decideAsk("completed", "h-1");
  await tick();
  assert.deepEqual(labels(doc), ["What did the command print?"]);
  assert.equal(inputs(doc).summary.required, true, "the worker resumes on a verdict");
  const intro = doc.querySelector("#dialogContent").textContent;
  assert.match(intro, /docker build -t piso \./);
  assert.match(intro, /\/workspace\/build\.log/);
});

for (const category of ["host", "dns"]) {
  test(`rejecting a ${category} human action accepts an empty note`, async (t) => {
    const { w, doc, calls } = await harness(t, [
      { kind: "human", id: "h-reject", category, title: "Human action", instructions: "Do this." },
    ]);
    await w.t.decideAsk("rejected", "h-reject");
    assert.deepEqual(labels(doc), ["Why (optional)"]);
    assert.equal(inputs(doc).summary.required, false);
    assert.equal(doc.querySelector("#actionForm").checkValidity(), true);
    submit(w, doc);
    await tick();
    const post = calls.find((c) => c.method === "POST");
    assert.equal(post.path, "/api/v1/asks/human-actions/h-reject/resolve");
    assert.deepEqual(post.body, { status: "rejected", evidence: { summary: "" } });
  });
}

test("resolving a non-host human action keeps its own wording", async (t) => {
  const { w, doc } = await harness(t, [
    { kind: "human", id: "h-2", category: "dns", title: "Prove the domain", instructions: "Add the TXT record." },
  ]);
  await w.t.decideAsk("completed", "h-2");
  await tick();
  assert.deepEqual(labels(doc), ["What you did (optional)"]);
  assert.match(doc.querySelector("#dialogContent").textContent, /Add the TXT record\./);
});
