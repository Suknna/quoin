import { useCursorPages } from "@/hooks/use-cursor-pages";
import type { Page } from "../api";

/**
 * 知识库列表统一走游标翻页（useCursorPages）；本包装仅把 Page 的可空 items
 * 规整为非空，并保留旧的调用形状。
 */
export function usePagedList<T extends { id: string }>(
	fetchPage: (cursor?: string) => Promise<Page<T>>,
	suspended: boolean,
	fallbackError: string,
	resetKey: string = "",
) {
	return useCursorPages<T>(
		(cursor) =>
			fetchPage(cursor).then((page) => ({
				items: page.items ?? [],
				nextCursor: page.nextCursor,
			})),
		{ suspended, resetKey, fallbackError },
	);
}

/** 列表与详情共用的时间格式:非法输入原样返回,不抛错。 */
export function formatDateTime(iso: string) {
	const date = new Date(iso);
	if (Number.isNaN(date.getTime())) return iso;
	return date.toLocaleString("zh-CN", {
		month: "numeric",
		day: "numeric",
		hour: "2-digit",
		minute: "2-digit",
	});
}
