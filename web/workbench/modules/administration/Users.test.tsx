import "@testing-library/jest-dom/vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Users } from "./Users";

describe("Users", () => { it("sends enabled, role, displayName and row-version fences through the user API", async () => { const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ items: [{ id: "u1", username: "op", displayName: "Operator", role: "operator", enabled: true, authRevision: 1, rowVersion: 7, passwordChangeRequired: false, lastLoginAt: null }] }) }); vi.stubGlobal("fetch", fetchMock); render(<Users suspended={false} />); await screen.findByText("Operator"); fireEvent.click(screen.getByRole("button", { name: "停用" })); await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2)); expect(fetchMock.mock.calls[1][0]).toBe("/api/v1/admin/users/u1"); expect(JSON.parse(fetchMock.mock.calls[1][1].body)).toMatchObject({ enabled: false, expectedRowVersion: 7 }); }); });
