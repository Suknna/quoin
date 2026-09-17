export type StructuredParse = { ok: true; value: unknown } | { ok: false };

/**
 * Mechanical client-side detection of a complete structured report. Only
 * whole JSON objects/arrays qualify; prose, primitives, and truncated
 * payloads stay in their original form — nothing is inferred or repaired.
 */
export function parseStructured(raw: string): StructuredParse {
  const text = raw.trim();
  if (!text.startsWith("{") && !text.startsWith("[")) return { ok: false };
  try {
    const parsed: unknown = JSON.parse(text);
    if (typeof parsed !== "object" || parsed === null) return { ok: false };
    return { ok: true, value: parsed };
  } catch {
    return { ok: false };
  }
}
