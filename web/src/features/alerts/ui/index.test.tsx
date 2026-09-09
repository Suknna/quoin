import "@testing-library/jest-dom/vitest";
import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { useAlertsModule } from "./index";

function View({ route }: { route: string }) {
  const view = useAlertsModule({
    user: { id: "1", username: "admin", displayName: "Admin", role: "admin", passwordChangeRequired: false, authRevision: 1, enabled: true, lastLoginAt: null, rowVersion: 1 },
    route,
    navigate: vi.fn(),
    suspended: false,
    openEvidence: vi.fn(),
  });
  return <>{view.content}</>;
}

describe("alerts module", () => {
  it("renders the history view when the route includes its query string", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => ({ items: [] }) }));
    render(<View route="/alerts?view=history" />);
    expect(await screen.findByRole("heading", { name: "告警历史" })).toBeInTheDocument();
  });
});
