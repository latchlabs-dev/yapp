import test from "node:test";
import assert from "node:assert/strict";
import { findModel, flattenModels, modelKey, modelLabel, parseModelKey } from "../src/lib/models.ts";

// The catalog endpoint serves the provider-grouped Pi-extension format. The
// console previously read a top-level `models`/`data` list that never existed,
// so the model picker was always empty.
test("flattenModels extracts models from the provider catalog", () => {
  const models = flattenModels({
    providers: [
      {
        name: "corp",
        base_url: "/deepseek/v1",
        api_type: "deepseek-completions",
        models: [
          { id: "corp-deepseek-v4-flash", name: "DeepSeek V4 Flash", reasoning: true, input: ["text"] },
          { id: "corp-deepseek-v4-pro", name: "DeepSeek V4 Pro", reasoning: true, input: ["text"] },
        ],
      },
    ],
  });

  assert.equal(models.length, 2);
  assert.equal(models[0]!.id, "corp-deepseek-v4-flash");
  assert.equal(models[0]!.provider, "corp");
  assert.deepEqual(models[0]!.input, ["text"]);
});

// Built-in providers have no route id; the proxy resolves those turns by model
// name, so an empty provider id is valid and must not drop the model.
test("flattenModels keeps models whose provider has no route id", () => {
  const models = flattenModels({
    providers: [{ name: "OpenAI", models: [{ id: "gpt-x" }] }],
  });

  assert.equal(models.length, 1);
  assert.equal(models[0]!.provider_id, "");
  assert.equal(models[0]!.provider, "OpenAI");
});

test("flattenModels uses the route id when present and tolerates empty input", () => {
  const models = flattenModels({
    providers: [
      { name: "corp", route_id: "acct_123", models: [{ id: "a", name: "A" }] },
      { name: "empty", models: [] },
    ],
  });

  assert.equal(models[0]!.provider_id, "acct_123");
  assert.deepEqual(flattenModels(undefined), []);
  assert.deepEqual(flattenModels({}), []);
});

// Two provider accounts can advertise the same model id. Selecting by id alone
// sends the turn to whichever account is first in the catalog, which is not the
// one the user picked.
test("model keys carry the provider account, not just the model id", () => {
  const models = flattenModels({
    providers: [
      { name: "First", route_id: "acct_1", models: [{ id: "shared", name: "Shared" }] },
      { name: "Second", route_id: "acct_2", models: [{ id: "shared", name: "Shared" }] },
    ],
  });

  const keys = models.map(modelKey);
  assert.notEqual(keys[0], keys[1]);
  assert.deepEqual(parseModelKey(keys[1]!), { provider_id: "acct_2", id: "shared" });
  assert.equal(findModel(models, "shared", "acct_2")!.provider, "Second");
  assert.equal(findModel(models, "shared", "acct_1")!.provider, "First");
});

// A model id can itself contain punctuation, so the key has to round-trip
// whatever the catalog advertises.
test("model keys round-trip ids containing a colon", () => {
  const key = modelKey({ id: "llama3:8b-instruct", provider_id: "acct_1" });
  assert.deepEqual(parseModelKey(key), { provider_id: "acct_1", id: "llama3:8b-instruct" });
});

// Built-in providers have no route id, and conversations stored before the
// provider was recorded carry none either. Both must still resolve.
test("findModel falls back to the model id when no provider matches", () => {
  const models = flattenModels({
    providers: [{ name: "OpenAI", models: [{ id: "gpt-x" }] }],
  });

  assert.equal(findModel(models, "gpt-x", "")!.id, "gpt-x");
  assert.equal(findModel(models, "gpt-x", "acct_gone")!.id, "gpt-x");
  assert.equal(findModel(models, undefined, "acct_1"), undefined);
  assert.equal(findModel(models, "missing", ""), undefined);
});

// The picker must distinguish duplicate ids, and must not clutter unique ones.
test("modelLabel names the provider only when the id is ambiguous", () => {
  const models = flattenModels({
    providers: [
      { name: "First", route_id: "acct_1", models: [{ id: "shared", name: "Shared" }, { id: "solo", name: "Solo" }] },
      { name: "Second", route_id: "acct_2", models: [{ id: "shared", name: "Shared" }] },
    ],
  });

  assert.equal(modelLabel(models[0]!, models), "Shared — First");
  assert.equal(modelLabel(models[2]!, models), "Shared — Second");
  assert.equal(modelLabel(models[1]!, models), "Solo");
});
