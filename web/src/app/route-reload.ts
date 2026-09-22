// 部署更新会把全部哈希 chunk 一次换新（dist 清空重建）：旧标签页里尚未加载过的
// 懒加载路由此刻仍请求旧文件名，而静态服务对缺失的 /assets/* 回落 index.html
// （200 + text/html），模块加载因 MIME 校验失败而拒绝。这里提供「整页刷新一次」
// 的恢复回路；守卫保证对真正损坏的部署不会无限自动刷新。
// 独立成模块是因为 jsdom 无法替换 location.reload（non-configurable），
// 回归测试需要一个可替换/可观察的边界。

export const STALE_ROUTE_RELOAD_KEY = "quoin.web.stale-route-reload";

const STALE_ROUTE_NOTE_ID = "quoin-stale-route-note";

/** 懒加载失败会卸载整棵 React 树，说明只能写在 React 之外的纯 DOM 里。 */
function noteStaleRoute(message: string): void {
	const existing = document.getElementById(STALE_ROUTE_NOTE_ID);
	if (existing) {
		existing.textContent = message;
		return;
	}
	const note = document.createElement("p");
	note.id = STALE_ROUTE_NOTE_ID;
	note.setAttribute("role", "status");
	note.className =
		"fixed inset-x-0 top-0 z-50 border-b bg-background p-3 text-center text-sm";
	// textContent 保持内容惰性，不关心错误对象里带什么。
	note.textContent = message;
	document.body.append(note);
}

/** 整页刷新以取回当前 index.html 与新 chunk；同一页面会话内只自动刷新一次。 */
export function reloadAfterStaleRoute(): void {
	if (window.sessionStorage.getItem(STALE_ROUTE_RELOAD_KEY)) {
		noteStaleRoute("页面资源仍无法加载，请手动刷新；若持续出现请联系管理员。");
		return;
	}
	window.sessionStorage.setItem(STALE_ROUTE_RELOAD_KEY, "1");
	noteStaleRoute("部署已更新，正在刷新页面…");
	window.location.reload();
}

/** 任一路由 chunk 成功加载即重新武装回路：下一次部署更新仍能自动恢复。 */
export function clearStaleRouteReloadGuard(): void {
	window.sessionStorage.removeItem(STALE_ROUTE_RELOAD_KEY);
}
