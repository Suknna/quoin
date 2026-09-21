/* eslint-disable react-hooks/exhaustive-deps -- 列表读取刻意只跟随 suspended/resetKey;fetchPage 经 ref 取最新。 */

import { useEffect, useRef, useState } from "react";
import { messageOf } from "@/app/shared";
import type { Page } from "../api";

/**
 * 一个游标分页列表:首屏读第一页,“加载更多”追加。
 * 挂起时暂停读取并作废在途响应(世代号),恢复后重读第一页;
 * resetKey(如过滤条件)变化时重读第一页并重置游标——游标与服务端过滤集绑定。
 */
export function usePagedList<T extends { id: string }>(
	fetchPage: (cursor?: string) => Promise<Page<T>>,
	suspended: boolean,
	fallbackError: string,
	resetKey: string = "",
) {
	const fetchRef = useRef(fetchPage);
	fetchRef.current = fetchPage;
	const suspendedRef = useRef(suspended);
	suspendedRef.current = suspended;
	const generationRef = useRef(0);
	// 挂起切换必须在渲染期作废旧世代,堵住 effect 清理前的落地窗口。
	const pauseScopeRef = useRef(suspended);
	if (pauseScopeRef.current !== suspended) {
		pauseScopeRef.current = suspended;
		generationRef.current += 1;
	}
	const [state, setState] = useState<{
		items: T[];
		nextCursor?: string;
		loading: boolean;
		loadingMore: boolean;
		error: string;
	}>({ items: [], loading: true, loadingMore: false, error: "" });
	async function load(cursor?: string) {
		if (suspendedRef.current) return;
		// 只有最新世代的响应可以写状态。
		const generation = generationRef.current + 1;
		generationRef.current = generation;
		setState((current) => ({
			items: cursor ? current.items : [],
			nextCursor: cursor ? current.nextCursor : undefined,
			error: "",
			loading: !cursor,
			loadingMore: Boolean(cursor),
		}));
		try {
			const page = await fetchRef.current(cursor);
			if (generation !== generationRef.current) return;
			setState((current) => {
				// 追加按 id 去重:游标窗口重叠与页内重复 id 都只留一行。
				const seen = new Set(current.items.map((item) => item.id));
				const fresh = page.items.filter((item) => {
					if (seen.has(item.id)) return false;
					seen.add(item.id);
					return true;
				});
				return {
					...current,
					items: cursor ? [...current.items, ...fresh] : page.items,
					nextCursor: page.nextCursor,
					error: "",
					loading: false,
					loadingMore: false,
				};
			});
		} catch (reason) {
			if (generation !== generationRef.current) return;
			setState((current) => ({
				...current,
				error: messageOf(reason, fallbackError),
				loading: false,
				loadingMore: false,
			}));
		}
	}
	// biome-ignore lint/correctness/useExhaustiveDependencies: load 每次渲染重建;只在挂起切换或过滤(resetKey)变化时重读。
	useEffect(() => {
		if (!suspended) void load();
		else {
			// 挂起:作废在途请求、收起加载指示;已读内容只读保留。
			generationRef.current += 1;
			setState((current) => ({
				...current,
				loading: false,
				loadingMore: false,
			}));
		}
	}, [suspended, resetKey]);
	// 卸载后的迟到响应不允许落地。
	useEffect(() => {
		return () => {
			generationRef.current += 1;
		};
	}, []);
	return { ...state, load };
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
