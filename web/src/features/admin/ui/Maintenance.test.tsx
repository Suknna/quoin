import "@testing-library/jest-dom/vitest";
import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Maintenance } from "./Maintenance";

describe("Maintenance", () => { it("renders SafeBlocking items and only the Upgrade drain allowlist", async () => { vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => ({ active: true, reason: "Upgrade", rowVersion: 4, items: [{ kind: "run", objectKey: "r1", safeState: "Blocking", detailCode: "running|cancel:inspection_run:run-9:6" }] }) })); render(<Maintenance authenticationSuspended={false} />); expect(await screen.findByText("Blocking")).toBeInTheDocument(); expect(screen.getByRole("button", { name: "取消排空" })).toBeInTheDocument(); expect(screen.getByRole("button", { name: "退出维护" })).toBeDisabled(); expect(screen.queryByRole("button", { name: /force|skip/i })).not.toBeInTheDocument(); }); });
