/** Schema-driven settings vocabulary for per-instance event source settings
 * (ADR-0014 story 2). The plugin catalog ships a closed JSON Schema per
 * event source kind; common scalar/array shapes render as native fields and
 * anything outside the vocabulary falls back to a raw JSON editor — the UI
 * never invents plugin-specific branches. All values are non-secret by
 * construction: credential material is rejected at schema registration. */

/** One renderable settings field projected from the plugin schema. */
export interface SettingsFieldSpec {
	key: string;
	kind: "string" | "enum" | "number" | "boolean" | "stringArray" | "json";
	required: boolean;
	description?: string;
	maxLength?: number;
	maxItems?: number;
	enumValues?: string[];
}

/** Editable draft value of one field: text for scalar/array/JSON shapes,
 * a checkbox state for booleans. */
export type SettingsDraftValue = string | boolean;
export type SettingsDraft = Record<string, SettingsDraftValue>;

function isRecord(value: unknown): value is Record<string, unknown> {
	return typeof value === "object" && value !== null && !Array.isArray(value);
}

function positiveInt(value: unknown): number | undefined {
	return typeof value === "number" && Number.isInteger(value) && value > 0
		? value
		: undefined;
}

function stringEnum(
	property: Record<string, unknown>,
): string[] | undefined {
	if (
		Array.isArray(property.enum) &&
		property.enum.length > 0 &&
		property.enum.every((value) => typeof value === "string")
	)
		return property.enum as string[];
	return undefined;
}

function fieldKind(property: Record<string, unknown>): SettingsFieldSpec["kind"] {
	if (stringEnum(property)) return "enum";
	if (property.type === "boolean") return "boolean";
	if (property.type === "number" || property.type === "integer")
		return "number";
	if (property.type === "string") return "string";
	if (
		property.type === "array" &&
		isRecord(property.items) &&
		property.items.type === "string"
	)
		return "stringArray";
	return "json";
}

/** Projects the plugin's closed settings schema into renderable field specs.
 * An empty list means the kind accepts only the empty settings document, so
 * the UI renders no settings affordance at all. */
export function settingsFieldSpecs(schema: unknown): SettingsFieldSpec[] {
	if (!isRecord(schema) || !isRecord(schema.properties)) return [];
	const required = Array.isArray(schema.required)
		? schema.required.filter((key): key is string => typeof key === "string")
		: [];
	const specs: SettingsFieldSpec[] = [];
	for (const [key, raw] of Object.entries(schema.properties)) {
		const property = isRecord(raw) ? raw : {};
		specs.push({
			key,
			kind: fieldKind(property),
			required: required.includes(key),
			description:
				typeof property.description === "string"
					? property.description
					: undefined,
			maxLength: positiveInt(property.maxLength),
			maxItems: positiveInt(property.maxItems),
			enumValues: stringEnum(property),
		});
	}
	return specs;
}

/** Initial blank draft for the given fields. */
export function emptyDraft(specs: SettingsFieldSpec[]): SettingsDraft {
	return Object.fromEntries(
		specs.map((spec) => [spec.key, spec.kind === "boolean" ? false : ""]),
	);
}

/** Serializes the stored settings document into editable draft values.
 * Unknown stored shapes degrade into their JSON text instead of being hidden. */
export function draftFromSettings(
	specs: SettingsFieldSpec[],
	settings: Record<string, unknown> | undefined,
): SettingsDraft {
	const draft = emptyDraft(specs);
	for (const spec of specs) {
		const value = settings?.[spec.key];
		if (spec.kind === "boolean") {
			if (typeof value === "boolean") draft[spec.key] = value;
			continue;
		}
		if (value === undefined || value === null) continue;
		if (spec.kind === "number") draft[spec.key] = String(value);
		else if (spec.kind === "stringArray")
			draft[spec.key] = Array.isArray(value)
				? value.map(String).join("\n")
				: JSON.stringify(value);
		else if (spec.kind === "json") draft[spec.key] = JSON.stringify(value, null, 2);
		else draft[spec.key] = String(value);
	}
	return draft;
}

export type SettingsValidation =
	| { ok: true; settings: Record<string, unknown> }
	| { ok: false; error: string };

/** Client-side pre-validation mirroring the closed schema vocabulary:
 * required presence, scalar parsing, one-item-per-line arrays and JSON
 * parseability. The server re-validates authoritatively before commit. */
export function settingsFromDraft(
	specs: SettingsFieldSpec[],
	draft: SettingsDraft,
): SettingsValidation {
	const settings: Record<string, unknown> = {};
	for (const spec of specs) {
		const value = draft[spec.key];
		if (spec.kind === "boolean") {
			// Booleans are always present: explicit false is a valid closed-schema value.
			settings[spec.key] = value === true;
			continue;
		}
		const text = typeof value === "string" ? value.trim() : "";
		if (!text) {
			if (spec.required)
				return { ok: false, error: `“${spec.key}”为必填设置。` };
			// Optional empty fields stay absent from the document.
			continue;
		}
		if (spec.kind === "number") {
			const parsed = Number(text);
			if (!Number.isFinite(parsed))
				return { ok: false, error: `“${spec.key}”必须是数字。` };
			settings[spec.key] = parsed;
			continue;
		}
		if (spec.kind === "stringArray") {
			const items = text
				.split("\n")
				.map((line) => line.trim())
				.filter(Boolean);
			if (items.length === 0) {
				if (spec.required)
					return { ok: false, error: `“${spec.key}”为必填设置。` };
				continue;
			}
			if (spec.maxItems !== undefined && items.length > spec.maxItems)
				return { ok: false, error: `“${spec.key}”最多 ${spec.maxItems} 项。` };
			const tooLong =
				spec.maxLength !== undefined
					? items.find((item) => item.length > spec.maxLength!)
					: undefined;
			if (tooLong !== undefined)
				return {
					ok: false,
					error: `“${spec.key}”的单项长度不能超过 ${spec.maxLength} 个字符。`,
				};
			settings[spec.key] = items;
			continue;
		}
		if (spec.maxLength !== undefined && text.length > spec.maxLength)
			return {
				ok: false,
				error: `“${spec.key}”长度不能超过 ${spec.maxLength} 个字符。`,
			};
		if (spec.kind === "json") {
			try {
				settings[spec.key] = JSON.parse(text);
			} catch {
				return { ok: false, error: `“${spec.key}”不是合法 JSON。` };
			}
			continue;
		}
		if (spec.kind === "enum" && spec.enumValues && !spec.enumValues.includes(text))
			return {
				ok: false,
				error: `“${spec.key}”必须是 ${spec.enumValues.join(" / ")} 之一。`,
			};
		settings[spec.key] = text;
	}
	return { ok: true, settings };
}

/** Human display of one stored settings value; objects/arrays print as JSON. */
export function formatSettingValue(value: unknown): string {
	return typeof value === "string" ? value : JSON.stringify(value);
}
