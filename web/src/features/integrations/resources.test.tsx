import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { IntegrationResources } from "./resources";

afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
});

it("shows source observations and opens inspection without loading business systems", async () => {
	const fetchMock = vi
		.spyOn(globalThis, "fetch")
		.mockImplementation(async (input) => {
			if (String(input).includes("/resources?"))
				return Response.json({
					items: [
						{
							id: "42",
							objectType: "target",
							identityKey: "target-one",
							displayName: "mall-mysql-exporter",
							labels: { job: "mysql", instance: "10.0.0.1:9104" },
							identityLabels: { job: "mysql", instance: "10.0.0.1:9104" },
							state: "observed",
							lastObservedAt: "2026-09-13T08:00:00Z",
						},
					],
				});
			return Response.json({ items: [] });
		});
	const navigate = vi.fn();
	render(
		<IntegrationResources
			connectionName="lab-prometheus"
			navigate={navigate}
			suspended={false}
		/>,
	);
	expect(await screen.findByText("mall-mysql-exporter")).toBeInTheDocument();
	expect(screen.getByText("当前观测到")).toBeInTheDocument();
	fireEvent.click(screen.getByRole("button", { name: "mall-mysql-exporter" }));
	expect(navigate).toHaveBeenCalledWith(
		"/integrations/prometheus/lab-prometheus/resources/42",
	);
	fireEvent.click(screen.getByRole("button", { name: "立即巡检" }));
	expect(navigate).toHaveBeenCalledWith(
		"/inspections?connectionName=lab-prometheus",
	);
	expect(
		fetchMock.mock.calls.every(
			([url]) => !String(url).includes("business-systems"),
		),
	).toBe(true);
});
