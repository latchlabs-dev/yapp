import type { Model, ProvidersResponse } from "./types";

/**
 * flattenModels turns the Pi-extension catalog response into the flat list the
 * console renders.
 *
 * The endpoint serves the same provider-grouped shape as /proxy/v1/models so
 * the console can only ever offer models the tenant is entitled to; grouping is
 * a provider-registration detail the UI does not need. The console's own
 * catalog is additionally narrowed server-side to the protocols a console turn
 * can invoke, so everything that arrives here is usable.
 *
 * `route_id` is absent for built-in providers, so the provider id can be empty
 * — the proxy resolves those turns by model name alone.
 */
export function flattenModels(resp: ProvidersResponse | undefined | null): Model[] {
  const models: Model[] = [];
  for (const provider of resp?.providers ?? []) {
    for (const model of provider.models ?? []) {
      models.push({
        ...model,
        id: model.id,
        provider_id: provider.route_id ?? "",
        provider: provider.name,
      });
    }
  }
  return models;
}

// Two provider accounts may advertise the same model id — the same model bought
// twice, or a tenant alias reused. The pair is therefore the selection identity:
// selecting by id alone silently sends the turn to whichever account is first.
//
// The separator is split on its first occurrence and provider ids never contain
// it, so a model id that does ("llama3:8b") still round-trips.
const KEY_SEPARATOR = "::";

export function modelKey(model: Pick<Model, "id" | "provider_id">): string {
  return `${model.provider_id ?? ""}${KEY_SEPARATOR}${model.id}`;
}

export function parseModelKey(key: string): { id: string; provider_id: string } {
  const at = key.indexOf(KEY_SEPARATOR);
  if (at < 0) return { id: key, provider_id: "" };
  return { provider_id: key.slice(0, at), id: key.slice(at + KEY_SEPARATOR.length) };
}

/**
 * findModel resolves a stored conversation's model/provider pair against the
 * catalog.
 *
 * A conversation saved before a provider id was recorded — or one on a built-in
 * provider, which has no route id — matches on the model id alone. That
 * fallback is deliberately last: it is the ambiguous case.
 */
export function findModel(
  models: Model[],
  id: string | undefined,
  providerId: string | undefined,
): Model | undefined {
  if (!id) return undefined;
  const exact = models.find((m) => m.id === id && m.provider_id === (providerId ?? ""));
  if (exact) return exact;
  return models.find((m) => m.id === id);
}

/**
 * modelLabel names a model for the picker. The provider account is part of the
 * label whenever another account offers the same model id, because otherwise
 * the two options read as the same choice.
 */
export function modelLabel(model: Model, models: Model[]): string {
  const name = model.name || model.id;
  const duplicated = models.filter((m) => m.id === model.id).length > 1;
  if (!duplicated || !model.provider) return name;
  return `${name} — ${model.provider}`;
}
