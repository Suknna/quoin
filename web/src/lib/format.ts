/** Shared locale formatter for server timestamps; invalid input stays visible as the fallback. */
export function formatDateTime(
	value: string | null | undefined,
	fallback = "—",
): string {
	if (!value) return fallback;
	const date = new Date(value);
	return Number.isNaN(date.getTime()) ? fallback : date.toLocaleString("zh-CN");
}
