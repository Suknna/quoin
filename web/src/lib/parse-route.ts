/** Parse a workspace route into its path and query, tolerating relative input. */
export function parseRoute(route: string): {
	pathname: string;
	searchParams: URLSearchParams;
} {
	const url = new URL(route, "https://workbench.invalid");
	return { pathname: url.pathname, searchParams: url.searchParams };
}
