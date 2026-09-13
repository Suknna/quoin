import "@testing-library/jest-dom/vitest";
import { render, screen } from "@testing-library/react";
import { lazy, Suspense } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { RouteHosts, type RouteHostComponents } from "./RouteHosts";

const user = { id: "1", username: "admin", displayName: "Admin", role: "admin" as const, passwordChangeRequired: false, authRevision: 1, enabled: true, lastLoginAt: null, rowVersion: 1 };
const Host = ({ props }: { props: { route: string } }) => <p>{props.route === "/alerts?view=history" ? "告警历史" : props.route}</p>;
const components = Object.fromEntries(["alerts", "investigations", "inspections", "systems", "knowledge", "account", "administration", "modelProvider", "integrations"].map((name) => [name, lazy(async () => ({ default: Host }))])) as RouteHostComponents;

beforeEach(() => {
  vi.stubGlobal("matchMedia", vi.fn().mockReturnValue({ matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() }));
});

describe("RouteHosts", () => {
	it("selects the alerts host when an alert view is encoded in the query string", async () => {
		render(<Suspense fallback={null}><RouteHosts components={components} props={{ user, route: "/alerts?view=history", navigate: vi.fn(), suspended: false, openEvidence: vi.fn() }} onLogout={vi.fn()} /></Suspense>);
		expect(await screen.findByText("告警历史")).toBeInTheDocument();
	});

	it("selects the separate model-provider settings host", async () => {
		render(<Suspense fallback={null}><RouteHosts components={components} props={{ user, route: "/admin/model_provider/new", navigate: vi.fn(), suspended: false, openEvidence: vi.fn() }} onLogout={vi.fn()} /></Suspense>);
		expect(await screen.findByText("/admin/model_provider/new")).toBeInTheDocument();
	});
});
