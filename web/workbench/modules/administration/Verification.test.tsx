import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Verification } from "./Verification";

const detail = { id: "9", applicableSetDigest: "app", catalogDigest: "catalog", deadlineAt: "2026-01-01", deploymentConfigDigest: "config", itemCount: 1, itemSetDigest: "items", manifestDigest: "manifest", progress: { completed: 0, total: 1 }, publicOriginDigest: "origin", releaseSubjectDigest: "release", resultProfileDigest: "profile", startedAt: "2026-01-01", items: [{ id: "1", cellId: "cell", inputDigest: "input", itemSeq: 1, objectKind: "deployment", scenarioId: "scenario" }], results: [{ id: "r1", itemId: "1", inputDigest: "input", category: "passed", committedAt: "x", evidenceIndexDigest: "e", observedAt: "x", outcome: "passed", producerType: "runtime", resultDigest: "r" }], conflicts: [{ id: "c1", itemId: "1", firstResultId: "r1", conflictingResultId: "r2", createdAt: "x" }], subjectDrifts: [{ itemId: "1", currentDigest: "current", frozenDigest: "frozen", driftField: "config_version", objectKind: "deployment", observedAt: "x" }] };

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("Verification", () => {
  it("loads real detail stages, provenance, conflicts, and drift", async () => {
    const fetchMock = vi.fn().mockImplementation((path: string) => Promise.resolve({ ok: true, json: async () => path === "/api/v1/deployment-verifications?limit=30" ? { items: [detail] } : detail }));
    vi.stubGlobal("fetch", fetchMock);
    render(<Verification suspended={false} />);
    await screen.findByText("9");
    fireEvent.click(screen.getByRole("button", { name: "详情" }));
    expect(await screen.findByText("结果冲突")).toBeInTheDocument();
    expect(screen.getByText("主体漂移")).toBeInTheDocument();
    expect(screen.getByText("runtime")).toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledWith("/api/v1/deployment-verifications/9", expect.anything());
  });

  it("renders the JSON null collections returned for a new verification without crashing", async () => {
    // Go's nil slices serialize as null; a freshly started invocation has no results, conflicts, or subject drifts yet.
    const accepted = { ...detail, id: "10", items: detail.items, results: null, conflicts: null, subjectDrifts: null };
    const fetchMock = vi.fn().mockImplementation((path: string) => Promise.resolve({ ok: true, json: async () => path === "/api/v1/deployment-verifications?limit=30" ? { items: [] } : accepted }));
    vi.stubGlobal("fetch", fetchMock);
    render(<Verification suspended={false} />);
    fireEvent.click(await screen.findByRole("button", { name: "开始验证" }));
    expect(await screen.findByRole("heading", { name: "验证详情 10" })).toBeInTheDocument();
  });
});
