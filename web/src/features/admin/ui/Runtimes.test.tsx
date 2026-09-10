import "@testing-library/jest-dom/vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Runtimes } from "./Runtimes";

const runtime = { slot: "plinth", state: "registered", currentGeneration: 2, pendingGeneration: 3, retiringGeneration: 1, retirementState: "PendingRetirement", rowVersion: 8, connected: true };
describe("Runtimes", () => {
 it("confirms replacement registration and uses the expected row-version fence", async () => {
  const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ plinth: runtime, lintel: { ...runtime, slot: "lintel" } }) }); vi.stubGlobal("fetch", fetchMock);
  render(<Runtimes suspended={false} />);
  await screen.findAllByRole("button", { name: "准备替代注册" });
  fireEvent.click(screen.getAllByRole("button", { name: "准备替代注册" })[0]);
  expect(screen.getByRole("heading", { name: "替代 plinth 的注册凭据？" })).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "确认" }));
  await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
  expect(fetchMock.mock.calls[1][0]).toBe("/api/v1/runtime-slots/plinth/registration/prepare");
  expect(JSON.parse(fetchMock.mock.calls[1][1].body)).toMatchObject({ expectedRowVersion: 8 });
 });
 it("labels an unregistered slot as first registration", async () => {
  const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ plinth: { ...runtime, state: "unregistered" }, lintel: { ...runtime, slot: "lintel" } }) }); vi.stubGlobal("fetch", fetchMock);
  render(<Runtimes suspended={false} />);
  expect(await screen.findByRole("button", { name: "准备首次注册" })).toBeInTheDocument();
 });
 it("clears a revealed secret explicitly", async () => {
  const fetchMock = vi.fn().mockImplementation((path: string) => path === "/api/v1/runtime" ? Promise.resolve({ ok: true, json: async () => ({ plinth: runtime, lintel: { ...runtime, slot: "lintel" } }) }) : path.includes("prepare") ? Promise.resolve({ ok: true, json: async () => ({ registrationTokenAvailable: true, registrationTokenHandle: "x".repeat(32) }) }) : Promise.resolve({ ok: true, json: async () => ({ registrationToken: "secret" }) })); vi.stubGlobal("fetch", fetchMock);
  render(<Runtimes suspended={false} />);
  fireEvent.click((await screen.findAllByRole("button", { name: "准备替代注册" }))[0]);
  fireEvent.click(screen.getByRole("button", { name: "确认" }));
  expect(await screen.findByText(/一次性注册令牌/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "关闭并清除" }));
  expect(screen.queryByText(/一次性注册令牌/)).not.toBeInTheDocument();
 });
 it("does not restore an asynchronously revealed secret after suspension", async () => {
  let reveal: (response: unknown) => void = () => {};
  const fetchMock = vi.fn().mockImplementation((path: string) => path === "/api/v1/runtime" ? Promise.resolve({ ok: true, json: async () => ({ plinth: runtime, lintel: { ...runtime, slot: "lintel" } }) }) : path.includes("prepare") ? Promise.resolve({ ok: true, json: async () => ({ registrationTokenAvailable: true, registrationTokenHandle: "x".repeat(32) }) }) : new Promise(resolve => { reveal = resolve; })); vi.stubGlobal("fetch", fetchMock);
  const view = render(<Runtimes suspended={false} />);
  await screen.findAllByRole("button", { name: "准备替代注册" });
  fireEvent.click(screen.getAllByRole("button", { name: "准备替代注册" })[0]);
  fireEvent.click(screen.getByRole("button", { name: "确认" }));
  await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
  view.rerender(<Runtimes suspended />);
  reveal({ ok: true, json: async () => ({ registrationToken: "secret" }) });
  await waitFor(() => expect(screen.queryByText(/一次性注册令牌/)).not.toBeInTheDocument());
 });
});
