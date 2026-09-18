import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { lazy, Suspense } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { type RouteHostComponents, RouteHosts } from "./RouteHosts";

const user = {
	id: "1",
	username: "admin",
	displayName: "Admin",
	role: "admin" as const,
	passwordChangeRequired: false,
	authRevision: 1,
	enabled: true,
	initialized: true,
	lastLoginAt: null,
	rowVersion: 1,
};
const Host = ({ props }: { props: { route: string } }) => (
	<p>{props.route === "/alerts?view=history" ? "告警历史" : props.route}</p>
);
const components = Object.fromEntries(
	[
		"alerts",
		"investigations",
		"inspections",
		"systems",
		"knowledge",
		"settings",
		"integrations",
	].map((name) => [name, lazy(async () => ({ default: Host }))]),
) as RouteHostComponents;

beforeEach(() => {
	vi.stubGlobal(
		"matchMedia",
		vi.fn().mockReturnValue({
			matches: false,
			addEventListener: vi.fn(),
			removeEventListener: vi.fn(),
		}),
	);
});

afterEach(cleanup);

describe("RouteHosts", () => {
	it("selects the alerts host when an alert view is encoded in the query string", async () => {
		render(
			<Suspense fallback={null}>
				<RouteHosts
					components={components}
					props={{
						user,
						route: "/alerts?view=history",
						navigate: vi.fn(),
						suspended: false,
						openEvidence: vi.fn(),
					}}
					onLogout={vi.fn()}
				/>
			</Suspense>,
		);
		expect(await screen.findByText("告警历史")).toBeInTheDocument();
	});

	it("keeps model providers inside the settings host, including detail deep links", async () => {
		render(
			<Suspense fallback={null}>
				<RouteHosts
					components={components}
					props={{
						user,
						route: "/settings/platform/model-providers/models%2Fmain",
						navigate: vi.fn(),
						suspended: false,
						openEvidence: vi.fn(),
					}}
					onLogout={vi.fn()}
				/>
			</Suspense>,
		);
		expect(
			await screen.findByText(
				"/settings/platform/model-providers/models%2Fmain",
			),
		).toBeInTheDocument();
	});

	it("routes the settings surface to the settings host for admins and operators alike", async () => {
		const adminView = render(
			<Suspense fallback={null}>
				<RouteHosts
					components={components}
					props={{
						user,
						route: "/settings/profile",
						navigate: vi.fn(),
						suspended: false,
						openEvidence: vi.fn(),
					}}
					onLogout={vi.fn()}
				/>
			</Suspense>,
		);
		expect(await screen.findByText("/settings/profile")).toBeInTheDocument();
		adminView.unmount();
		render(
			<Suspense fallback={null}>
				<RouteHosts
					components={components}
					props={{
						user: { ...user, role: "operator" as const },
						route: "/settings/security",
						navigate: vi.fn(),
						suspended: false,
						openEvidence: vi.fn(),
					}}
					onLogout={vi.fn()}
				/>
			</Suspense>,
		);
		expect(await screen.findByText("/settings/security")).toBeInTheDocument();
		expect(screen.queryByText("无权访问此页面")).not.toBeInTheDocument();
	});

	it("forbids operators from the platform settings group", async () => {
		render(
			<Suspense fallback={null}>
				<RouteHosts
					components={components}
					props={{
						user: { ...user, role: "operator" as const },
						route: "/settings/platform/users",
						navigate: vi.fn(),
						suspended: false,
						openEvidence: vi.fn(),
					}}
					onLogout={vi.fn()}
				/>
			</Suspense>,
		);
		expect(await screen.findByText("无权访问此页面")).toBeInTheDocument();
	});
});
