import "@testing-library/jest-dom/vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { workbenchApi } from "@/api/workbench";
import { ModelProviderEditor } from "./ModelProviderEditor";

vi.mock("@/api/workbench", async (importOriginal) => ({
	...(await importOriginal<typeof import("@/api/workbench")>()),
	workbenchApi: { createConnection: vi.fn() },
}));

afterEach(() => vi.clearAllMocks());

describe("ModelProviderEditor", () => {
	it("allows chat-only providers and omits a blank embedding model", async () => {
		vi.mocked(workbenchApi.createConnection).mockResolvedValue({ name: "deepseek", type: "model_provider", enabled: false, revalidationRequired: false, rowVersion: 1, config: {} });
		render(<ModelProviderEditor onCreated={vi.fn()} readOnly={false} />);
		fireEvent.change(screen.getByLabelText("名称"), { target: { value: "deepseek" } });
		fireEvent.change(screen.getByLabelText("Base URL"), { target: { value: "https://api.deepseek.example" } });
		fireEvent.change(screen.getByLabelText("API Key"), { target: { value: "secret" } });
		fireEvent.change(screen.getByLabelText("对话模型 ID"), { target: { value: "deepseek-chat" } });
		fireEvent.change(screen.getByLabelText("Context 预算 tokens"), { target: { value: "4096" } });
		fireEvent.change(screen.getByLabelText("最大输出 tokens"), { target: { value: "1024" } });
		fireEvent.click(screen.getByRole("button", { name: "创建模型提供方" }));
		expect(await screen.findByRole("button", { name: "创建模型提供方" })).toBeInTheDocument();
		expect(workbenchApi.createConnection).toHaveBeenCalledWith("deepseek", expect.objectContaining({ type: "model_provider", chatModelId: "deepseek-chat" }));
		expect(vi.mocked(workbenchApi.createConnection).mock.calls[0][1]).not.toHaveProperty("embeddingModelId");
	});
});
