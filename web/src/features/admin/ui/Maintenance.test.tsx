import "@testing-library/jest-dom/vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Maintenance } from "./Maintenance";

describe("Maintenance", () => {
 it("renders SafeBlocking items and only the Upgrade drain allowlist", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => ({ active: true, reason: "Upgrade", rowVersion: 4, items: [{ kind: "run", objectKey: "r1", safeState: "Blocking", detailCode: "running|cancel:inspection_run:run-9:6" }] }) }));
  render(<Maintenance authenticationSuspended={false} />);
  expect(await screen.findByText("Blocking")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "取消排空" })).toBeInTheDocument();
  expect(screen.queryByText("没有维护清单项目")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "退出维护" })).toBeDisabled();
  expect(screen.queryByRole("button", { name: /force|skip/i })).not.toBeInTheDocument();
 });
 it("uses the Empty component when there are no maintenance items", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => ({ active: false, rowVersion: 7, items: [] }) }));
  render(<Maintenance authenticationSuspended={false} />);
  expect(await screen.findByText("没有维护清单项目")).toBeInTheDocument();
 });
 it("prepares an Upgrade maintenance session when maintenance is inactive", async () => {
  const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ active: false, rowVersion: 7, items: [] }) }); vi.stubGlobal("fetch", fetchMock);
  render(<Maintenance authenticationSuspended={false} />);
  fireEvent.click(await screen.findByRole("button", { name: "准备升级" }));
  fireEvent.click(screen.getByRole("button", { name: "确认" }));
  expect(fetchMock.mock.calls[1][0]).toBe("/api/v1/maintenance/upgrade/prepare");
  expect(JSON.parse(fetchMock.mock.calls[1][1].body)).toMatchObject({ expectedRowVersion: 7 });
 });
});
