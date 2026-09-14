import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer, type IncomingMessage } from "node:http";
import type { AddressInfo } from "node:net";
import { mintPortalIdentity, PORTAL_IDENTITY_HEADER } from "../../chassis/src/portal-identity.ts";

function isNoncedCoreCall(url: string, pathname: string): boolean {
  const u = new URL(url, "http://core");
  if (u.pathname !== pathname) return false;
  const keys = [...u.searchParams.keys()];
  return (
    keys.length === 1 && keys[0] === "_sourceAuthNonce" && (u.searchParams.get("_sourceAuthNonce") ?? "").length > 0
  );
}

interface Call {
  method: string;
  url: string;
  body: Record<string, unknown>;
}

const calls: Call[] = [];
const project = {
  id: "p1",
  name: "Launch",
  ownerId: "alice",
  memberIds: ["alice"],
  scopeId: "group:web-project-p1",
  members: [{ principalId: "alice", displayName: "Alice" }],
};
const core = createServer((req: IncomingMessage, res) => {
  let raw = "";
  req.on("data", (chunk) => (raw += chunk));
  req.on("end", () => {
    const body = raw ? (JSON.parse(raw) as Record<string, unknown>) : {};
    calls.push({ method: req.method ?? "GET", url: req.url ?? "", body });
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify(req.method === "GET" ? { projects: [project] } : { project }));
  });
});
await new Promise<void>((resolve) => core.listen(0, resolve));

process.env.CORE_API_URL = `http://localhost:${(core.address() as AddressInfo).port}`;
process.env.CORE_SIGNING_SECRET = "projects-web-route-test";
process.env.WEB_UI_PRINCIPALS = "alice";

const { handler } = await import("../server/index.ts");
const surface = createServer((req, res) => void handler(req, res));
await new Promise<void>((resolve) => surface.listen(0, resolve));
const base = `http://localhost:${(surface.address() as AddressInfo).port}`;
const headers = {
  [PORTAL_IDENTITY_HEADER]: mintPortalIdentity({ p: "alice", exp: Date.now() + 60_000 }, "projects-web-route-test"),
  "content-type": "application/json",
};

test.after(() => {
  surface.close();
  core.close();
});

test("project routes bind the signed-in principal and relay canonical scope and member labels", async () => {
  let before = calls.length;
  await fetch(`${base}/api/projects`, {
    method: "POST",
    headers,
    body: JSON.stringify({ name: "  Launch  ", principalId: "mallory" }),
  });
  assert.deepEqual(calls.slice(before).find((call) => isNoncedCoreCall(call.url, "/v1/projects"))?.body, {
    principalId: "alice",
    name: "Launch",
  });

  before = calls.length;
  await fetch(`${base}/api/projects/p1/members`, {
    method: "POST",
    headers,
    body: JSON.stringify({ memberId: "bob", principalId: "mallory" }),
  });
  assert.deepEqual(calls.slice(before).find((call) => isNoncedCoreCall(call.url, "/v1/projects/p1/members"))?.body, {
    principalId: "alice",
    memberId: "bob",
  });

  before = calls.length;
  await fetch(`${base}/api/projects/p1`, {
    method: "PATCH",
    headers,
    body: JSON.stringify({ name: "  Renamed  ", principalId: "mallory" }),
  });
  assert.deepEqual(calls.slice(before).find((call) => isNoncedCoreCall(call.url, "/v1/projects/p1"))?.body, {
    principalId: "alice",
    name: "Renamed",
  });

  before = calls.length;
  await fetch(`${base}/api/projects/p1/members/bob`, { method: "DELETE", headers });
  assert.deepEqual(
    calls.slice(before).find((call) => isNoncedCoreCall(call.url, "/v1/projects/p1/members/bob"))?.body,
    {
      principalId: "alice",
    },
  );

  before = calls.length;
  await fetch(`${base}/api/projects/p1/files`, {
    method: "POST",
    headers,
    body: JSON.stringify({ fileId: "file-1", principalId: "mallory" }),
  });
  assert.deepEqual(calls.slice(before).find((call) => isNoncedCoreCall(call.url, "/v1/projects/p1/files"))?.body, {
    principalId: "alice",
    fileId: "file-1",
  });

  before = calls.length;
  await fetch(`${base}/api/projects/p1/files/file-1/retry`, { method: "POST", headers });
  assert.deepEqual(
    calls.slice(before).find((call) => isNoncedCoreCall(call.url, "/v1/projects/p1/files/file-1/retry"))?.body,
    { principalId: "alice" },
  );

  before = calls.length;
  await fetch(`${base}/api/projects/p1/files/file-1`, { method: "DELETE", headers });
  assert.deepEqual(
    calls.slice(before).find((call) => isNoncedCoreCall(call.url, "/v1/projects/p1/files/file-1"))?.body,
    { principalId: "alice" },
  );

  before = calls.length;
  await fetch(`${base}/api/projects/p1/knowledge/documents?path=${encodeURIComponent("wiki/entities/唐僧.md")}`, {
    headers,
  });
  const sourceCall = calls
    .slice(before)
    .find((call) => new URL(call.url, "http://core").pathname === "/v1/projects/p1/knowledge/documents");
  const sourceURL = new URL(sourceCall?.url ?? "", "http://core");
  assert.equal(sourceCall?.method, "GET");
  assert.equal(sourceURL.searchParams.get("viewer"), "alice");
  assert.equal(sourceURL.searchParams.get("path"), "wiki/entities/唐僧.md");
  assert.ok(sourceURL.searchParams.get("_sourceAuthNonce"));
});
