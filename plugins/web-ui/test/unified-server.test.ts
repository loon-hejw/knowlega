import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer, type IncomingMessage } from "node:http";
import type { AddressInfo } from "node:net";

const coreRequests: string[] = [];
let forwardedPortalIdentity: string | null = null;
const core = createServer((req: IncomingMessage, res) => {
  const path = new URL(req.url ?? "/", "http://core.local").pathname;
  coreRequests.push(path);
  forwardedPortalIdentity = (req.headers["x-portal-identity"] as string) ?? null;
  res.setHeader("content-type", "application/json");
  if (path === "/v1/surface-config") {
    res.end(JSON.stringify({ modelProviderConfigured: true, branding: { selfLabel: "Unified test" } }));
    return;
  }
  if (path === "/v1/admin/whoami") {
    res.end(JSON.stringify({ isAdmin: true, role: "org_admin", scopeId: "org:acme" }));
    return;
  }
  res.end(JSON.stringify({}));
});
await new Promise<void>((resolve) => core.listen(0, "127.0.0.1", resolve));

process.env.PORT = "0";
process.env.CORE_API_URL = `http://127.0.0.1:${(core.address() as AddressInfo).port}`;
process.env.CORE_ORG_ID = "acme";
process.env.CORE_SIGNING_SECRET = "unified-server-core-secret";
process.env.PORTAL_IDENTITY_SECRET = "unified-server-identity-secret";
process.env.PORTAL_SESSION_SECRET = "unified-server-session-secret";
process.env.PORTAL_PUBLIC_URL = "http://localhost:0";
process.env.PORTAL_LOCAL_AUTH_BYPASS = "1";
process.env.PORTAL_DEV_PRINCIPAL = "local-admin";
process.env.WEB_UI_PUBLIC_URL = "http://localhost:0";
process.env.WEB_UI_UPSTREAM = "http://127.0.0.1:1";
process.env.ADMIN_UPSTREAM = "http://127.0.0.1:1";
process.env.ADMIN_BASE_PATH = "/admin";
process.env.NODE_ENV = "test";

const { server } = await import("../server/unified.ts");
await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;

test.after(() => {
  server.close();
  core.close();
});

test("the unified listener serves health, web root, and admin root without surface upstreams", async () => {
  const health = await fetch(`${base}/healthz`);
  assert.equal(health.status, 200);
  assert.deepEqual(await health.json(), { ok: true });

  const login = await fetch(`${base}/auth/login?returnTo=/`, { redirect: "manual" });
  assert.equal(login.status, 302);
  assert.equal(login.headers.get("location"), "/");
  const cookie = (login.headers.get("set-cookie") ?? "").split(";", 1)[0];
  assert.match(cookie, /^portal_session=/);

  const web = await fetch(`${base}/`, { headers: { cookie, accept: "text/html" } });
  assert.equal(web.status, 200);
  assert.match(await web.text(), /Unified test/);

  const admin = await fetch(`${base}/admin/`, { headers: { cookie, accept: "text/html" } });
  assert.equal(admin.status, 200);
  assert.match(await admin.text(), /Admin/);

  const whoami = await fetch(`${base}/admin/api/whoami`, { headers: { cookie } });
  assert.equal(whoami.status, 200);
  assert.equal((await whoami.json()).isAdmin, true);
  assert.ok(coreRequests.includes("/v1/admin/whoami"));

  const scopes = await fetch(`${base}/admin/api/scopes`, { headers: { cookie } });
  assert.equal(scopes.status, 200);
  assert.ok(forwardedPortalIdentity, "embedded admin forwards the signed portal identity to core");
});
