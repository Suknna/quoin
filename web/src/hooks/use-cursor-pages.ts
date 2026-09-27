import { useCallback, useEffect, useRef, useState } from "react";
import { messageOf } from "@/app/shared";

export interface CursorPage<T> {
	items: T[];
	nextCursor?: string;
}

/**
 * 不透明游标分页（HTTP-PAGE-001：无总数、无上一步游标）。翻页替换而非追加；
 * “上一页”靠客户端记住到达每一页所用的游标栈。resetKey（筛选等）变化时
 * 回第一页；suspended 期间作废在途响应并暂停读取。
 */
export function useCursorPages<T>(
	fetchPage: (cursor?: string) => Promise<CursorPage<T>>,
	options: {
		suspended?: boolean;
		resetKey?: string;
		fallbackError: string;
		/** 默认挂载/筛选变化即自动读第一页；页面已有统一 load() 驱动时置 false。 */
		autoLoad?: boolean;
	},
) {
	const { suspended = false, resetKey = "", fallbackError, autoLoad = true } = options;
	const fetchRef = useRef(fetchPage);
	fetchRef.current = fetchPage;
	const generationRef = useRef(0);
	/** 最近一次取数的（游标、历史），失败重试时原样重跑。 */
	const lastAttemptRef = useRef<{ cursor?: string; history: string[] }>({
		cursor: undefined,
		history: [],
	});
	const [state, setState] = useState<{
		items: T[];
		nextCursor?: string;
		/** 到达第 2..N 页所用的游标；空 = 第一页。 */
		history: string[];
		loading: boolean;
		navigating: boolean;
		error: string;
	}>({ items: [], history: [], loading: true, navigating: false, error: "" });

	const load = useCallback(
		async (cursor: string | undefined, history: string[]) => {
			// 只有最新世代的响应可以写状态。
			const generation = ++generationRef.current;
			lastAttemptRef.current = { cursor, history };
			setState((current) => ({
				...current,
				loading: current.items.length === 0 && history.length === 0,
				navigating: true,
				error: "",
			}));
			try {
				const page = await fetchRef.current(cursor);
				if (generation !== generationRef.current) return;
				setState({
					items: page.items,
					nextCursor: page.nextCursor,
					history,
					loading: false,
					navigating: false,
					error: "",
				});
			} catch (reason) {
				if (generation !== generationRef.current) return;
				setState((current) => ({
					...current,
					error: messageOf(reason, fallbackError),
					loading: false,
					navigating: false,
				}));
			}
		},
		[fallbackError],
	);

	useEffect(() => {
		if (suspended) {
			// 挂起：作废在途响应、收起加载指示；已读内容只读保留。
			generationRef.current += 1;
			setState((current) => ({ ...current, loading: false, navigating: false }));
			return;
		}
		if (autoLoad) void load(undefined, []);
	}, [suspended, resetKey, load, autoLoad]);
	// 卸载后的迟到响应不允许落地。
	useEffect(
		() => () => {
			generationRef.current += 1;
		},
		[],
	);

	const goNext = useCallback(
		() =>
			state.nextCursor
				? load(state.nextCursor, [...state.history, state.nextCursor])
				: Promise.resolve(),
		[load, state.nextCursor, state.history],
	);
	const goPrev = useCallback(() => {
		const history = state.history.slice(0, -1);
		return load(history.at(-1), history);
	}, [load, state.history]);
	const refresh = useCallback(() => load(undefined, []), [load]);
	const retry = useCallback(
		() => load(lastAttemptRef.current.cursor, lastAttemptRef.current.history),
		[load],
	);

	return {
		items: state.items,
		/** 1-based 当前页码。 */
		page: state.history.length + 1,
		hasPrev: state.history.length > 0,
		hasNext: Boolean(state.nextCursor),
		loading: state.loading,
		navigating: state.navigating,
		error: state.error,
		goNext,
		goPrev,
		/** 回第一页重读；供保存后刷新与轮询复用。 */
		refresh,
		/** 重跑最近一次取数（含失败的翻页），不改变当前页码。 */
		retry,
	};
}
