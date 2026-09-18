import "@testing-library/jest-dom/vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Backups } from "./Backups";

const settings = { enabled: true, scheduleCron: "0 2 * * *", timezone: "Asia/Shanghai", backupTarget: "local", retentionCount: 7, rowVersion: 3 };
const retention = { generatedRetentionDays: 14, rowVersion: 5 };
const page = { items: [{ id: "42", status: "succeeded", stage: "complete", createdAt: "now", updatedAt: "now", sizeBytes: 10 }] };
function response(body: unknown, ok = true, status = 200) { return { ok, status, json: async () => body } as Response; }
function responses(fetch: ReturnType<typeof vi.fn>) { fetch.mockResolvedValueOnce(response(page)).mockResolvedValueOnce(response(settings)).mockResolvedValueOnce(response(retention)); }
afterEach(() => vi.unstubAllGlobals());
describe("Backups", () => {
  it("saves backup settings with its expected row-version fence", async () => {
    const fetch = vi.fn(); responses(fetch); fetch.mockResolvedValueOnce(response({ ...settings, retentionCount: 9, rowVersion: 4 })); vi.stubGlobal("fetch", fetch);
    render(<Backups suspended={false}/>);
    const input = await screen.findByLabelText("保留份数"); fireEvent.change(input, { target: { value: "9" } }); fireEvent.click(screen.getByRole("button", { name: "保存备份设置" }));
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(4));
    const [path, init] = fetch.mock.calls[3]; expect(path).toBe("/api/v1/backups/settings"); expect(JSON.parse(init.body)).toMatchObject({ expectedRowVersion: 3, retentionCount: 9 });
  });
  it("renders the same-origin download URL only for successful backups", async () => {
    const fetch = vi.fn(); responses(fetch); vi.stubGlobal("fetch", fetch);
    render(<Backups suspended={false}/>);
    expect((await screen.findByRole("link", { name: "下载归档" })).getAttribute("href")).toBe("/api/v1/backups/42/download");
  });
});
