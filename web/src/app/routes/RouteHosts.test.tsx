import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { lazy, Suspense } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RouteHosts, type RouteHostComponents } from "./RouteHosts";

const user = { id: "1", username: "admin", displayName: "Admin", role: "admin" as const, passwordChangeRequired: false, authRevision: 1, enabled: true, initialized: true, lastLoginAt: null, rowVersion: 1 };
const Host = ({ props }: { props: { route: string } }) => <p>{props.route === "/alerts?view=history" ? "告警历史" : props.route}</p>;
const components = Object.fromEntries(["alerts", "investigations", "inspections", "systems", "knowledge", "account", "administration", "modelProvider", "integrations"].map((name) => [name, lazy(async () => ({ default: Host }))])) as RouteHostComponents;

beforeEach(() => {
  vi.stubGlobal("matchMedia", vi.fn().mockReturnValue({ matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() }));
});

afterEach(cleanup);

describe("RouteHosts", () => {
	it("selects the alerts host when an alert view is encoded in the query string", async () => {
		render(<Suspense fallback={null}><RouteHosts components={components} props={{ user, route: "/alerts?view=history", navigate: vi.fn(), suspended: false, openEvidence: vi.fn() }} onLogout={vi.fn()} /></Suspense>);
		expect(await screen.findByText("告警历史")).toBeInTheDocument();
	});

	it("selects the separate model-provider settings host", async () => {
		render(<Suspense fallback={null}><RouteHosts components={components} props={{ user, route: "/admin/model_provider/new", navigate: vi.fn(), suspended: false, openEvidence: vi.fn() }} onLogout={vi.fn()} /></Suspense>);
		expect(await screen.findByText("/admin/model_provider/new")).toBeInTheDocument();
	});

	it("routes the account contact-change view to the account host for admins and operators alike", async () => {
		const adminView = render(<Suspense fallback={null}><RouteHosts components={components} props={{ user, route: "/account/contacts", navigate: vi.fn(), suspended: false, openEvidence: vi.fn() }} onLogout={vi.fn()} /></Suspense>);
		expect(await screen.findByText("/account/contacts")).toBeInTheDocument();
		adminView.unmount();
		render(<Suspense fallback={null}><RouteHosts components={components} props={{ user: { ...user, role: "operator" as const }, route: "/account/contacts", navigate: vi.fn(), suspended: false, openEvidence: vi.fn() }} onLogout={vi.fn()} /></Suspense>);
		expect(await screen.findByText("/account/contacts")).toBeInTheDocument();
		expect(screen.queryByText("无权访问此页面")).not.toBeInTheDocument();
	});
});
