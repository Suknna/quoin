import { useEffect, useRef } from "react";

/**
 * Shared polling lifecycle for server-owned projections. The callback always
 * sees the latest closure via a ref, so callers can pass an inline function
 * without re-arming the interval every render. Polling pauses entirely while
 * `active` is false (suspension) and never resumes by itself.
 */
export function usePolling(
	callback: () => void,
	intervalMs: number,
	active: boolean,
): void {
	const callbackRef = useRef(callback);
	useEffect(() => {
		callbackRef.current = callback;
	});
	useEffect(() => {
		if (!active) return;
		const timer = window.setInterval(() => callbackRef.current(), intervalMs);
		return () => window.clearInterval(timer);
	}, [intervalMs, active]);
}
