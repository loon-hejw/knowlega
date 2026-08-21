import { tool } from "@opencode-ai/plugin";

function requiredEnv(name) {
  const value = globalThis.process.env[name]?.trim();
  if (!value) throw new Error(`${name} is required`);
  return value;
}

function loopbackBridgeUrl(raw) {
  const url = new URL(raw.endsWith("/") ? raw : `${raw}/`);
  const hostname = url.hostname.toLowerCase();
  if (url.protocol !== "http:" || (hostname !== "localhost" && hostname !== "[::1]" && !hostname.startsWith("127."))) {
    throw new Error("OPENCODE_BRIDGE_URL must be an HTTP loopback URL");
  }
  return url;
}

function schemaWithMetadata(schema, value) {
  let result = value;
  if (schema.description) result = result.describe(schema.description);
  if (schema.default !== undefined) result = result.default(schema.default);
  return result;
}

function union(z, members) {
  if (members.length === 0) return z.never();
  if (members.length === 1) return members[0];
  return z.union(members);
}

function literal(z, value) {
  if (value === null || ["string", "number", "boolean"].includes(typeof value)) return z.literal(value);
  throw new Error(`Unsupported JSON Schema literal: ${JSON.stringify(value)}`);
}

function stringSchema(z, schema) {
  let value = z.string();
  if (schema.minLength !== undefined) value = value.min(schema.minLength);
  if (schema.maxLength !== undefined) value = value.max(schema.maxLength);
  if (schema.pattern !== undefined) value = value.regex(new RegExp(schema.pattern));
  return value;
}

function numberSchema(z, schema, integer) {
  let value = integer ? z.number().int() : z.number();
  if (schema.minimum !== undefined) value = value.min(schema.minimum);
  if (schema.maximum !== undefined) value = value.max(schema.maximum);
  return value;
}

function objectSchema(z, schema) {
  const properties = schema.properties ?? {};
  const required = new Set(schema.required ?? []);
  const shape = Object.fromEntries(Object.entries(properties).map(([name, property]) => {
    const value = jsonSchemaToZod(z, property);
    return [name, required.has(name) ? value : value.optional()];
  }));
  const patterns = Object.values(schema.patternProperties ?? {});
  if (Object.keys(properties).length === 0 && patterns.length === 1) {
    return z.record(z.string(), jsonSchemaToZod(z, patterns[0]));
  }
  const value = z.object(shape);
  if (schema.additionalProperties === false) return value.strict();
  if (typeof schema.additionalProperties === "object") return value.catchall(jsonSchemaToZod(z, schema.additionalProperties));
  return value.passthrough();
}

function jsonSchemaToZod(z, schema) {
  const alternatives = schema.anyOf ?? schema.oneOf;
  if (alternatives) return schemaWithMetadata(schema, union(z, alternatives.map((member) => jsonSchemaToZod(z, member))));
  if (schema.const !== undefined) return schemaWithMetadata(schema, literal(z, schema.const));
  if (schema.enum) return schemaWithMetadata(schema, union(z, schema.enum.map((member) => literal(z, member))));
  if (Array.isArray(schema.type)) {
    return schemaWithMetadata(schema, union(z, schema.type.map((type) => jsonSchemaToZod(z, { ...schema, type, description: undefined, default: undefined }))));
  }
  let value;
  switch (schema.type) {
    case "object": value = objectSchema(z, schema); break;
    case "array":
      value = z.array(jsonSchemaToZod(z, schema.items ?? {}));
      if (schema.minItems !== undefined) value = value.min(schema.minItems);
      if (schema.maxItems !== undefined) value = value.max(schema.maxItems);
      break;
    case "string": value = stringSchema(z, schema); break;
    case "integer": value = numberSchema(z, schema, true); break;
    case "number": value = numberSchema(z, schema, false); break;
    case "boolean": value = z.boolean(); break;
    case "null": value = z.null(); break;
    case undefined: value = schema.properties || schema.patternProperties ? objectSchema(z, schema) : z.unknown(); break;
    default: throw new Error(`Unsupported JSON Schema type: ${schema.type}`);
  }
  return schemaWithMetadata(schema, value);
}

function argumentShape(z, schema) {
  if (schema.type !== "object" && !schema.properties) throw new Error("Tool parameters must be an object schema");
  const required = new Set(schema.required ?? []);
  return Object.fromEntries(Object.entries(schema.properties ?? {}).map(([name, property]) => {
    const value = jsonSchemaToZod(z, property);
    return [name, required.has(name) ? value : value.optional()];
  }));
}

function sessionIDFromMessages(messages) {
  for (let index = messages.length - 1; index >= 0; index -= 1) {
    const sessionID = messages[index]?.info.sessionID;
    if (sessionID) return sessionID;
  }
  return undefined;
}

function needsHistoryImport(messages, history) {
  return history !== undefined && messages.length === 1;
}

const OpenCodeBridgePlugin = async ({ client }) => {
  const bridgeUrl = loopbackBridgeUrl(requiredEnv("OPENCODE_BRIDGE_URL"));
  const bridgeSecret = requiredEnv("OPENCODE_BRIDGE_SECRET");
  const request = async (path, init) => {
    const response = await fetch(new URL(path, bridgeUrl), {
      ...init,
      headers: {
        authorization: `Bearer ${bridgeSecret}`,
        ...(init?.body ? { "content-type": "application/json" } : {}),
        ...(init?.headers ?? {}),
      },
    });
    const text = await response.text();
    if (!response.ok) throw new Error(`OpenCode bridge ${path} failed: ${response.status} ${text}`);
    return JSON.parse(text);
  };
  const discovered = await request("definitions");
  const bridgeTools = Array.isArray(discovered) ? discovered : discovered.tools;
  const tools = Object.fromEntries(bridgeTools.map((definition) => [definition.name, tool({
    description: definition.description,
    args: argumentShape(tool.schema, definition.parameters),
    async execute(args, context) {
      const callID = context.callID;
      if (!callID) throw new Error("OpenCode omitted the bridged tool call ID");
      const result = await request(`session/${encodeURIComponent(context.sessionID)}/tool`, {
        method: "POST",
        body: JSON.stringify({ tool: definition.name, sessionID: context.sessionID, callID, args }),
      });
      if (result.terminate) await client.session.abort({ path: { id: context.sessionID } }).catch(() => undefined);
      return result.output;
    },
  })]));
  const sessionContext = async (sessionID) => {
    let error;
    for (let attempt = 0; attempt < 20; attempt += 1) {
      try {
        return await request(`session/${encodeURIComponent(sessionID)}/context`);
      } catch (next) {
        error = next;
        await new Promise((resolve) => setTimeout(resolve, 25));
      }
    }
    throw error;
  };
  return {
    tool: tools,
    "chat.headers": async (input, output) => {
      const context = await sessionContext(input.sessionID);
      Object.assign(output.headers, context.proxyHeaders ?? {});
    },
    "experimental.chat.system.transform": async (input, output) => {
      if (!input.sessionID) return;
      const context = await sessionContext(input.sessionID);
      if (context.systemPrompt !== undefined) output.system.splice(0, output.system.length, context.systemPrompt);
    },
    "experimental.chat.messages.transform": async (_input, output) => {
      const currentLastUser = output.messages.findLast((message) => message.info.role === "user");
      const sessionID = sessionIDFromMessages(output.messages);
      if (!sessionID || !currentLastUser) return;
      const context = await sessionContext(sessionID);
      const history = context.history ?? context.messages;
      if (needsHistoryImport(output.messages, history)) output.messages.splice(0, output.messages.length, ...(history ?? []), currentLastUser);
      await request(`session/${encodeURIComponent(sessionID)}/capture`, {
        method: "POST",
        body: JSON.stringify({ system: context.systemPrompt ?? "", messages: output.messages }),
      });
    },
  };
};

export default OpenCodeBridgePlugin;
