import "@testing-library/jest-dom/vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { LabelContracts } from "./LabelContracts";

const contract = { id: "11", version: 11, state: "draft", rowVersion: 7, parserVersion: "p", schemaVersion: "s", createdAt: "now" };
const readiness = { targetContractVersion: 11, stateRowVersion: 4, targetRowVersion: 7, currentContractVersionId: "10", systems: [{ businessSystemKey: "payments", currentConfigVersionId: "8", businessSystemRowVersion: 9, blockers: [], activationCandidates: [{ configVersionId: "12", passedVerificationRunId: "33" }] }] };
function response(body: unknown, ok = true, status = 200) { return { ok, status, json: async () => body } as Response; }

afterEach(() => vi.unstubAllGlobals());
describe("LabelContracts", () => {
  it("submits the selected YAML file as multipart and surfaces server field errors", async () => {
    const fetch = vi.fn().mockResolvedValueOnce(response({ items: [contract] })).mockResolvedValueOnce(response({ message: "invalid", fieldErrors: [{ path: "file", reason: "不是有效 YAML" }] }, false, 422));
    vi.stubGlobal("fetch", fetch);
    render(<LabelContracts suspended={false}/>);
    const file = new File(["bad"], "contract.yaml", { type: "application/yaml" });
    fireEvent.change(await screen.findByLabelText("Label Contract YAML"), { target: { files: [file] } });
    fireEvent.click(screen.getByRole("button", { name: "上传契约" }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("file: 不是有效 YAML"));
    const [, init] = fetch.mock.calls[1];
    expect(init.method).toBe("POST");
    expect(init.body).toBeInstanceOf(FormData);
    expect((init.body as FormData).get("file")).toMatchObject({ name: "contract.yaml" });
  });

  it("sends all activation version fences and the selected verification ID", async () => {
    const fetch = vi.fn()
      .mockResolvedValueOnce(response({ items: [contract] }))
      .mockResolvedValueOnce(response({ ...contract, yamlBody: "kind: LabelContract" }))
      .mockResolvedValueOnce(response(readiness))
      .mockResolvedValueOnce(response({ ...contract, state: "active" }))
      .mockResolvedValueOnce(response({ items: [contract] }))
      .mockResolvedValueOnce(response({ ...contract, state: "active", yamlBody: "kind: LabelContract" }))
      .mockResolvedValueOnce(response(readiness));
    vi.stubGlobal("fetch", fetch);
    render(<LabelContracts suspended={false}/>);
    fireEvent.click(await screen.findByRole("button", { name: "查看与就绪性" }));
    await screen.findByText("payments");
    fireEvent.click(screen.getByRole("button", { name: "原子激活" }));
    fireEvent.click(await screen.findByRole("button", { name: "确认激活" }));
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(7));
    const [, init] = fetch.mock.calls[3];
    expect(JSON.parse(init.body)).toMatchObject({ expectedStateRowVersion: 4, expectedTargetRowVersion: 7, expectedCurrentContractVersionId: "10", compatibleVersions: [{ businessSystemKey: "payments", configVersionId: "12", verificationRunId: "33", expectedCurrentConfigVersionId: "8", expectedBusinessSystemRowVersion: 9 }] });
  });
});
