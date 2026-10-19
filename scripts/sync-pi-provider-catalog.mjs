#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const namespace = process.env.CATALOG_NAMESPACE || "pi";
const dsn = process.env.DSN || process.env.PROXY_POSTGRES_DSN;
const GPT_DISCOUNT_MULTIPLIER = 0.10;
const emitSQL = process.argv.includes("--sql");

if (!dsn && !emitSQL) {
  console.error("DSN or PROXY_POSTGRES_DSN is required");
  process.exit(1);
}

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const modelsPath = resolveModelsPath(repoRoot);
const { MODELS } = await import(pathToFileURL(modelsPath).href);
const sourceVersion = readPackageVersion(modelsPath);

if (!MODELS || typeof MODELS !== "object") {
  console.error(`Pi catalog at ${modelsPath} did not export MODELS`);
  process.exit(1);
}

const providerRows = [];
const modelRows = [];

// Keep newly supported models discoverable while the installed Pi catalog
// catches up. A bundled Pi entry takes precedence over these fallbacks.
const supplementalModels = {
  "openai-codex": [{
    id: "gpt-6-astra",
    name: "GPT-6 Astra",
    api: "openai-codex-responses",
    provider: "openai-codex",
    baseUrl: "https://chatgpt.com/backend-api",
    reasoning: true,
    input: ["text", "image"],
    thinkingLevelMap: { off: null, minimal: "low", xhigh: "xhigh", max: "max" },
    cost: { input: 10, output: 50, cacheRead: 1, cacheWrite: 12.5 },
    contextWindow: 1050000,
    maxTokens: 128000,
    catalogSource: "https://developers.openai.com/api/docs/models/gpt-6-astra",
    catalogSourceVersion: "piproxy-astra-2026-09-05",
  }],
};

const zAICosts = {
  "glm-4.5-air": { input: 0.20, output: 1.10, cache_read: 0.03, cache_write: 0 },
  "glm-4.7": { input: 0.60, output: 2.20, cache_read: 0.11, cache_write: 0 },
  "glm-5-turbo": { input: 1.20, output: 4.00, cache_read: 0.24, cache_write: 0 },
  "glm-5.1": { input: 1.40, output: 4.40, cache_read: 0.26, cache_write: 0 },
  "glm-5.2": { input: 1.40, output: 4.40, cache_read: 0.26, cache_write: 0 },
  "glm-5v-turbo": { input: 1.20, output: 4.00, cache_read: 0.24, cache_write: 0 },
};

for (const [providerKey, modelGroup] of Object.entries(MODELS)) {
  const models = [...normalizeModelGroup(modelGroup)];
  for (const model of supplementalModels[providerKey] || []) {
    if (!models.some((existing) => existing.id === model.id)) {
      models.push(model);
    }
  }
  if (models.length === 0) {
    continue;
  }

  const first = models[0];
  const route = routeDefaults(providerKey, first);
  providerRows.push({
    namespace,
    providerKey,
    displayName: titleizeProvider(providerKey),
    apiType: first.api || route.apiType,
    baseURL: route.baseURL,
    publicBasePath: route.publicBasePath,
    publicAPIBasePath: route.publicAPIBasePath,
    allowedPaths: route.allowedPaths,
    metadata: {
      pi_provider: first.provider || providerKey,
      pi_api: first.api || "",
      pi_base_url: first.baseUrl || "",
    },
    sourceVersion,
  });

  for (const model of models) {
    const modelRoute = routeDefaults(providerKey, model);
    modelRows.push({
      namespace,
      providerKey,
      modelID: model.id,
      displayName: model.name || model.id,
      apiType: model.api || first.api || route.apiType,
      baseURL: modelRoute.baseURL,
      reasoning: Boolean(model.reasoning),
      input: Array.isArray(model.input) ? model.input : [],
      thinkingLevelMap: model.thinkingLevelMap ?? null,
      compat: normalizeCompat(model.compat),
      cost: normalizeCost(providerKey, model.id, model.cost),
      contextWindow: Number(model.contextWindow || 0),
      maxTokens: Number(model.maxTokens || 0),
      metadata: {
        pi_provider: model.provider || providerKey,
        pi_api: model.api || "",
        pi_base_url: model.baseUrl || "",
        ...(model.catalogSource ? { catalog_source: model.catalogSource } : {}),
      },
      sourceVersion: model.catalogSourceVersion || sourceVersion,
    });
  }
}

if (providerRows.length === 0) {
  console.error(`No provider rows found in ${modelsPath}`);
  process.exit(1);
}

const sql = buildSQL(providerRows, modelRows);
if (emitSQL) {
  process.stdout.write(sql);
  process.exit(0);
}

const result = spawnSync("psql", [dsn, "-v", "ON_ERROR_STOP=1"], {
  input: sql,
  encoding: "utf8",
  stdio: ["pipe", "inherit", "inherit"],
});

if (result.status !== 0) {
  process.exit(result.status ?? 1);
}

console.log(`Synced ${providerRows.length} Pi provider catalogs and ${modelRows.length} models from ${modelsPath}`);

function resolveModelsPath(root) {
  const candidates = [
    process.env.PI_AI_MODELS_PATH,
    path.join(root, "node_modules/@earendil-works/pi-ai/dist/models.generated.js"),
    "/opt/homebrew/lib/node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-ai/dist/models.generated.js",
    "/usr/local/lib/node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-ai/dist/models.generated.js",
    "/usr/lib/node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-ai/dist/models.generated.js",
  ].filter(Boolean);

  for (const candidate of candidates) {
    if (existsSync(candidate)) {
      return candidate;
    }
  }

  console.error("Could not find @earendil-works/pi-ai/dist/models.generated.js");
  console.error("Set PI_AI_MODELS_PATH to the generated model catalog path.");
  process.exit(1);
}

function readPackageVersion(modelsFile) {
  const packageFile = path.resolve(path.dirname(modelsFile), "..", "package.json");
  if (!existsSync(packageFile)) {
    return "";
  }
  try {
    const pkg = JSON.parse(readFileSync(packageFile, "utf8"));
    return pkg.version || "";
  } catch {
    return "";
  }
}

function routeDefaults(providerKey, model) {
  const apiType = model.api || "openai-completions";
  const rawBaseURL = trimSlash(model.baseUrl || "");

  if (providerKey === "deepseek" || apiType === "deepseek-completions") {
    return {
      apiType: "deepseek-completions",
      baseURL: rawBaseURL,
      publicBasePath: "/deepseek",
      publicAPIBasePath: "/deepseek/v1",
      allowedPaths: ["/v1/chat/completions", "/v1/models"],
    };
  }

  if (apiType === "anthropic-messages") {
    return {
      apiType,
      baseURL: rawBaseURL,
      publicBasePath: "/anthropic",
      publicAPIBasePath: "/anthropic/v1",
      allowedPaths: ["/v1/messages", "/v1/models"],
    };
  }

  if (apiType === "openai-completions") {
    if (rawBaseURL.endsWith("/v1")) {
      return {
        apiType,
        baseURL: rawBaseURL.slice(0, -3),
        publicBasePath: "/openai",
        publicAPIBasePath: "/openai/v1",
        allowedPaths: ["/v1/chat/completions", "/v1/models"],
      };
    }
    return {
      apiType,
      baseURL: rawBaseURL,
      publicBasePath: "/openai/v1",
      publicAPIBasePath: "/openai/v1",
      allowedPaths: ["/chat/completions", "/models"],
    };
  }

  return {
    apiType,
    baseURL: rawBaseURL,
    publicBasePath: `/${providerKey}`,
    publicAPIBasePath: `/${providerKey}/v1`,
    allowedPaths: [],
  };
}

function normalizeModelGroup(modelGroup) {
  if (Array.isArray(modelGroup)) {
    return modelGroup;
  }
  if (modelGroup && typeof modelGroup === "object") {
    return Object.values(modelGroup);
  }
  return [];
}

function normalizeCompat(compat) {
  if (!compat || typeof compat !== "object") {
    return null;
  }
  return compat;
}

function normalizeCost(providerKey, modelID, cost) {
  if (!cost || typeof cost !== "object") {
    const effective = costOverride(providerKey, modelID) ?? { input: 0, output: 0, cache_read: 0, cache_write: 0 };
    return applyProviderDiscount(providerKey, modelID, effective);
  }
  const normalized = {
    input: Number(cost.input || 0),
    output: Number(cost.output || 0),
    cache_read: Number(cost.cacheRead ?? cost.cache_read ?? 0),
    cache_write: Number(cost.cacheWrite ?? cost.cache_write ?? 0),
  };
  let effective = normalized;
  if (normalized.input === 0 && normalized.output === 0 && normalized.cache_read === 0 && normalized.cache_write === 0) {
    effective = costOverride(providerKey, modelID) ?? normalized;
  }
  return applyProviderDiscount(providerKey, modelID, effective);
}

function applyProviderDiscount(providerKey, modelID, cost) {
  if (providerKey !== "openai-codex" || !modelID.startsWith("gpt-")) {
    return cost;
  }
  return Object.fromEntries(Object.entries(cost).map(([key, value]) => [key, value * GPT_DISCOUNT_MULTIPLIER]));
}

function costOverride(providerKey, modelID) {
  if ((providerKey === "zai" || providerKey === "zai-coding-cn") && zAICosts[modelID]) {
    return zAICosts[modelID];
  }
  return null;
}

function buildSQL(providers, models) {
  const syncedKeys = providers.map((row) => row.providerKey);
  const chunks = [
    "BEGIN;",
    `DELETE FROM provider_catalog_models WHERE namespace = ${sqlText(namespace)} AND provider_key = ANY(${sqlTextArray(syncedKeys)});`,
  ];

  for (const row of providers) {
    chunks.push(`
INSERT INTO provider_catalogs (
  namespace, provider_key, display_name, api_type, base_url,
  public_base_path, public_api_base_path, allowed_paths, metadata,
  source_version, synced_at
) VALUES (
  ${sqlText(row.namespace)}, ${sqlText(row.providerKey)}, ${sqlText(row.displayName)},
  ${sqlText(row.apiType)}, ${sqlText(row.baseURL)}, ${sqlText(row.publicBasePath)},
  ${sqlText(row.publicAPIBasePath)}, ${sqlTextArray(row.allowedPaths)},
  ${sqlJSON(row.metadata)}, ${sqlText(row.sourceVersion)}, now()
)
ON CONFLICT (namespace, provider_key) DO UPDATE SET
  display_name = EXCLUDED.display_name,
  api_type = EXCLUDED.api_type,
  base_url = EXCLUDED.base_url,
  public_base_path = EXCLUDED.public_base_path,
  public_api_base_path = EXCLUDED.public_api_base_path,
  allowed_paths = EXCLUDED.allowed_paths,
  metadata = EXCLUDED.metadata,
  source_version = EXCLUDED.source_version,
  synced_at = EXCLUDED.synced_at;`);
  }

  for (const row of models) {
    chunks.push(`
INSERT INTO provider_catalog_models (
  namespace, provider_key, model_id, display_name, api_type, base_url,
  reasoning, input, thinking_level_map, compat, cost, context_window,
  max_tokens, metadata, source_version, synced_at
) VALUES (
  ${sqlText(row.namespace)}, ${sqlText(row.providerKey)}, ${sqlText(row.modelID)},
  ${sqlText(row.displayName)}, ${sqlText(row.apiType)}, ${sqlText(row.baseURL)},
  ${row.reasoning ? "true" : "false"}, ${sqlTextArray(row.input)},
  ${row.thinkingLevelMap === null ? "NULL" : sqlJSON(row.thinkingLevelMap)},
  ${row.compat === null ? "NULL" : sqlJSON(row.compat)},
  ${sqlJSON(row.cost)}, ${sqlInteger(row.contextWindow)}, ${sqlInteger(row.maxTokens)},
  ${sqlJSON(row.metadata)}, ${sqlText(row.sourceVersion)}, now()
)
ON CONFLICT (namespace, provider_key, model_id) DO UPDATE SET
  display_name = EXCLUDED.display_name,
  api_type = EXCLUDED.api_type,
  base_url = EXCLUDED.base_url,
  reasoning = EXCLUDED.reasoning,
  input = EXCLUDED.input,
  thinking_level_map = EXCLUDED.thinking_level_map,
  compat = EXCLUDED.compat,
  cost = EXCLUDED.cost,
  context_window = EXCLUDED.context_window,
  max_tokens = EXCLUDED.max_tokens,
  metadata = EXCLUDED.metadata,
  source_version = EXCLUDED.source_version,
  synced_at = EXCLUDED.synced_at;`);
  }

  chunks.push("COMMIT;");
  return chunks.join("\n");
}

function titleizeProvider(providerKey) {
  const known = {
    "zai": "ZAI",
    "zai-coding-cn": "ZAI Coding Plan (China)",
    "deepseek": "DeepSeek",
    "xiaomi-token-plan-sgp": "Xiaomi MiMo Token Plan (SGP)",
    "xiaomi-token-plan-cn": "Xiaomi MiMo Token Plan (CN)",
    "xiaomi-token-plan-ams": "Xiaomi MiMo Token Plan (AMS)",
  };
  if (known[providerKey]) {
    return known[providerKey];
  }
  return providerKey
    .split("-")
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(" ");
}

function trimSlash(value) {
  return value.replace(/\/+$/, "");
}

function sqlText(value) {
  return `'${String(value ?? "").replaceAll("'", "''")}'`;
}

function sqlInteger(value) {
  const n = Number(value);
  return Number.isFinite(n) ? String(Math.trunc(n)) : "0";
}

function sqlJSON(value) {
  return `${sqlText(JSON.stringify(value ?? {}))}::jsonb`;
}

function sqlTextArray(values) {
  const items = (values || []).map((value) => sqlText(value)).join(",");
  return `ARRAY[${items}]::text[]`;
}
