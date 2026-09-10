import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { alertmanagerReceiverYaml } from "./api";
import { useIntegrationsModule } from "./ui";
import type { WorkspaceModuleProps } from "@/app/module-contract";

const props: WorkspaceModuleProps = {
	user: { id: "admin-1", username: "admin", displayName: "Admin", role: "admin", passwordChangeRequired: false, authRevision: 1, enabled: true, lastLoginAt: null, rowVersion: 1 },
	route: "/integrations",
	navigate: vi.fn(),
	suspended: false,
	openEvidence: vi.fn(),
};

function IntegrationView({ route = "/integrations" }: { route?: string }) {
	const view = useIntegrationsModule({ ...props, route });
	return <>{view.content}</>;
}

afterEach(cleanup);

describe("integration workbench", () => {
	it("renders the supported catalog and never offers saves for unavailable platforms", () => {
		render(<IntegrationView />);
		expect(screen.getByRole("heading", { name: "接入管理" })).toBeInTheDocument();
		expect(screen.getByText("Alertmanager")).toBeInTheDocument();
		for (const name of ["Prometheus", "Thanos", "Kubernetes", "受控浏览器"]) {
			expect(screen.getByText(name)).toBeInTheDocument();
		}
		expect(screen.getAllByRole("button", { name: "尚未开放" })).toHaveLength(4);
	});

	it("filters the catalog by platform name", () => {
		render(<IntegrationView />);
		fireEvent.change(screen.getByRole("textbox", { name: "搜索支持的平台" }), { target: { value: "Thanos" } });
		expect(screen.getByText("Thanos")).toBeInTheDocument();
		expect(screen.queryByText("Kubernetes")).not.toBeInTheDocument();
	});

	it("shows denied content instead of a management view to an operator", () => {
		render(<IntegrationView />);
		cleanup();
		function OperatorView() {
			const view = useIntegrationsModule({ ...props, user: { ...props.user, role: "operator" } });
			return <>{view.content}</>;
		}
		render(<OperatorView />);
		expect(screen.getByRole("alert")).toHaveTextContent("接入管理仅向管理员开放");
	});

	it("generates a valid Alertmanager receiver YAML snippet", () => {
		const yaml = alertmanagerReceiverYaml("https://quoin.example.test/api/v1/alert-receiver", "secret-token");
		expect(yaml).toContain("webhook_configs:");
		expect(yaml).toContain("send_resolved: true");
		expect(yaml).toContain("type: Bearer");
		expect(yaml).toContain("credentials: \"secret-token\"");
	});

	it("creates an Alertmanager source and opens its in-memory reveal dialog", async () => {
		const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
			const url = String(input);
			if (url.endsWith("/api/v1/alert-sources")) return Response.json({ revealHandle: "one-time-handle", revealAvailable: true });
			if (url.endsWith("/api/v1/alert-sources/credentials/reveal")) return Response.json({ bearerToken: "secret-token" });
			if (url.endsWith("/api/v1/alert-sources/receiver-config")) return Response.json({ publicReceiverUrl: "https://quoin.example.test/api/v1/alert-receiver" });
			return Response.json({ message: "unexpected request" }, { status: 500 });
		});
		render(<IntegrationView route="/integrations/alertmanager" />);
		fireEvent.change(screen.getByLabelText("来源键"), { target: { value: "production" } });
		fireEvent.click(screen.getByRole("button", { name: "创建并显示一次凭据" }));
		await waitFor(() => expect(screen.getByRole("dialog")).toBeInTheDocument());
		expect(screen.getByDisplayValue("secret-token")).toBeInTheDocument();
		expect(screen.getByDisplayValue("https://quoin.example.test/api/v1/alert-receiver")).toBeInTheDocument();
		fetchMock.mockRestore();
	});
});
