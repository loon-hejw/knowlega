import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { pathToFileURL } from "node:url";
import { handler as webHandler, initializeEmbeddedWebUI } from "./index.ts";
import { handle as adminHandler, probeAdmin } from "../../admin/src/index.ts";
import { bootChecks, configureEmbeddedSurfaceHandlers, handle as portalHandler } from "../../portal/src/index.ts";

const PORT = Number(process.env.PORT ?? 8129);

export const server = createServer((req, res) => {
  void portalHandler(req, res).catch((err: unknown) => {
    console.error(`[frontend] 500 ${req.method ?? "?"} ${req.url ?? "?"}:`, err);
    if (!res.headersSent) {
      res.writeHead(500, { "content-type": "application/json" });
      res.end(JSON.stringify({ error: "internal_error" }));
    } else {
      res.end();
    }
  });
});

configureEmbeddedSurfaceHandlers({
  "web-ui": async (req: IncomingMessage, res: ServerResponse) => webHandler(req, res),
  admin: async (req: IncomingMessage, res: ServerResponse) => adminHandler(req, res),
  adminProbe: probeAdmin,
});

export async function startServer(): Promise<void> {
  bootChecks();
  await initializeEmbeddedWebUI(server);
  await new Promise<void>((resolve) => {
    server.listen(PORT, () => {
      console.log(`[frontend] unified surface on http://localhost:${PORT} → / and /admin/`);
      resolve();
    });
  });
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  startServer().catch((err: unknown) => {
    console.error("[frontend] failed to start:", err);
    process.exit(1);
  });
}
