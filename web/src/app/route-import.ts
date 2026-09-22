import type { ComponentType } from "react";
import {
	clearStaleRouteReloadGuard,
	reloadAfterStaleRoute,
} from "./route-reload";
import type { RouteHostProps } from "./routes/RouteHosts";

type RouteHostModule = { default: ComponentType<RouteHostProps> };

/**
 * 包裹路由 host 的动态 import：部署更新使旧 chunk 失效时，先安排一次受守卫的
 * 整页刷新再把原错误抛回。否则 React lazy 的失败没有任何错误边界接管，
 * 整棵工作台树会被静默卸载成空白页（验收 fix7 期间 /knowledge/candidates/:id
 * 白屏的实际根因，与候选数据无关）。
 */
export function importRouteHost(
	factory: () => Promise<RouteHostModule>,
): Promise<RouteHostModule> {
	return factory().then(
		(module) => {
			clearStaleRouteReloadGuard();
			return module;
		},
		(reason: unknown) => {
			reloadAfterStaleRoute();
			throw reason;
		},
	);
}
