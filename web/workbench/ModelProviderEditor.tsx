import { Check, ChevronsUpDown } from "lucide-react";
import { type FormEvent, useEffect, useRef, useState } from "react";
import { Button } from "../templates/components/ui/button";
import {
	Command,
	CommandEmpty,
	CommandGroup,
	CommandInput,
	CommandItem,
	CommandList,
} from "../templates/components/ui/command";
import {
	Field,
	FieldDescription,
	FieldLabel,
} from "../templates/components/ui/field";
import { Input } from "../templates/components/ui/input";
import {
	Popover,
	PopoverContent,
	PopoverTrigger,
} from "../templates/components/ui/popover";
import type { ConnectionSummaryView } from "./api";
import { workbenchApi } from "./api";
import { ErrorMessage, messageOf } from "./shared";

/** A discovery response is only a picker aid; it never asserts provider capability. */
function ModelPicker({
	id,
	label,
	value,
	onChange,
	models,
	disabled,
	required = false,
}: {
	id: string;
	label: string;
	value: string;
	onChange: (next: string) => void;
	models: { id: string; metadata?: Record<string, unknown> }[];
	disabled: boolean;
	required?: boolean;
}) {
	const [open, setOpen] = useState(false);
	return (
		<Field>
			<FieldLabel htmlFor={id}>{label}</FieldLabel>
			<div className="flex gap-2">
				<Input
					id={id}
					value={value}
					onChange={(event) => onChange(event.target.value)}
					disabled={disabled}
					required={required}
					placeholder="手工填写模型 ID"
				/>
				<Popover open={open} onOpenChange={setOpen}>
					<PopoverTrigger asChild>
						<Button type="button" variant="outline" disabled={disabled} aria-label={`${label}选择`}>
							<ChevronsUpDown />
						</Button>
					</PopoverTrigger>
					<PopoverContent className="w-72 p-0" align="end">
						<Command>
							<CommandInput placeholder="搜索已发现模型…" />
							<CommandList>
								<CommandEmpty>尚无可选择的已发现模型。</CommandEmpty>
								<CommandGroup heading="已发现模型">
									{models.map((model) => (
										<CommandItem
											key={model.id}
											value={model.id}
											onSelect={() => {
												onChange(model.id);
												setOpen(false);
											}}
										>
											<Check className={value === model.id ? "opacity-100" : "opacity-0"} />
											{model.id}
										</CommandItem>
									))}
								</CommandGroup>
							</CommandList>
						</Command>
					</PopoverContent>
				</Popover>
			</div>
		</Field>
	);
}

export function ModelProviderEditor({
	onCreated,
	readOnly,
	suspended = false,
}: {
	onCreated: (connection: ConnectionSummaryView) => void;
	readOnly: boolean;
	suspended?: boolean;
}) {
	const [name, setName] = useState("");
	const [baseUrl, setBaseUrl] = useState("");
	const [apiKey, setApiKey] = useState("");
	const [chatModelId, setChatModelId] = useState("");
	const [embeddingModelId, setEmbeddingModelId] = useState("");
	const [contextBudgetTokens, setContextBudgetTokens] = useState("");
	const [maxOutputTokens, setMaxOutputTokens] = useState("");
	const [models, setModels] = useState<{ id: string; metadata?: Record<string, unknown> }[]>([]);
	const [discoveryNote, setDiscoveryNote] = useState("");
	const [discovering, setDiscovering] = useState(false);
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState("");
	const discoveryKey = useRef(0);

	// Credentials must never survive a suspended authenticated workspace.
	useEffect(() => {
		if (suspended) {
			setApiKey("");
			setModels([]);
			setDiscoveryNote("");
      setDiscovering(false);
			discoveryKey.current += 1;
		}
	}, [suspended]);

	function invalidateDiscovery() {
    setDiscovering(false);
		discoveryKey.current += 1;
		setModels([]);
		setDiscoveryNote("");
	}

	async function discover() {
		const requestKey = discoveryKey.current + 1;
		discoveryKey.current = requestKey;
		setDiscovering(true);
		setError("");
		setModels([]);
		setDiscoveryNote("");
		try {
			const result = await workbenchApi.discoverProviderModels(baseUrl, apiKey);
			if (discoveryKey.current !== requestKey) return;
			if (result.available) {
				setModels(result.items);
				setDiscoveryNote(`发现 ${result.items.length} 个模型；仍可手工填写模型 ID。`);
			} else {
				setDiscoveryNote(`${result.detail ? `${result.detail}；` : ""}可以直接手工填写模型 ID。`);
			}
		} catch (reason) {
			if (discoveryKey.current === requestKey) setError(messageOf(reason, "模型发现请求未完成；可以直接手工填写模型 ID。"));
		} finally {
			if (discoveryKey.current === requestKey) setDiscovering(false);
		}
	}

	async function submit(event: FormEvent) {
		event.preventDefault();
		setError("");
		const contextBudget = Number(contextBudgetTokens);
		const maxOutput = Number(maxOutputTokens);
		if (!Number.isSafeInteger(contextBudget) || contextBudget < 1 || !Number.isSafeInteger(maxOutput) || maxOutput < 1) {
			setError("Context 预算和最大输出必须是大于 0 的整数。");
			return;
		}
		setSaving(true);
		try {
			onCreated(await workbenchApi.createConnection(name, {
				type: "model_provider",
				baseUrl,
				chatModelId,
				embeddingModelId,
				contextBudgetTokens: contextBudget,
				maxOutputTokens: maxOutput,
				apiKey,
			}));
			setApiKey("");
		} catch (reason) {
			setError(messageOf(reason, "暂时无法创建模型提供方连接。"));
		} finally {
			setSaving(false);
		}
	}

	const disabled = readOnly || saving || suspended;
	return (
		<form className="grid gap-4" onSubmit={submit}>
			<div>
				<h2 className="text-lg font-semibold">添加模型提供方</h2>
				<FieldDescription>API Key 仅随发现或创建请求发送，不缓存、不回显。</FieldDescription>
			</div>
			{error && <ErrorMessage>{error}</ErrorMessage>}
			<Field><FieldLabel htmlFor="model-connection-name">名称</FieldLabel><Input id="model-connection-name" value={name} onChange={(event) => setName(event.target.value)} disabled={disabled} required /></Field>
			<Field><FieldLabel htmlFor="model-base-url">Base URL</FieldLabel><Input id="model-base-url" type="url" value={baseUrl} onChange={(event) => { setBaseUrl(event.target.value); invalidateDiscovery(); }} disabled={disabled} required placeholder="https://api.example.com" /></Field>
			<Field><FieldLabel htmlFor="model-api-key">API Key</FieldLabel><Input id="model-api-key" type="password" autoComplete="new-password" value={apiKey} onChange={(event) => { setApiKey(event.target.value); invalidateDiscovery(); }} disabled={disabled} required /></Field>
			<div className="flex flex-wrap items-center gap-2"><Button type="button" variant="outline" onClick={() => void discover()} disabled={disabled || discovering || !baseUrl}>{discovering ? "正在发现…" : "发现模型"}</Button><span className="text-sm text-muted-foreground">发现仅辅助填写，不构成验证。</span></div>
			{discoveryNote && <p role="status" className="text-sm text-muted-foreground">{discoveryNote}</p>}
			<ModelPicker id="chat-model-id" label="对话模型 ID" value={chatModelId} onChange={setChatModelId} models={models} disabled={disabled} required />
			<ModelPicker id="embedding-model-id" label="Embedding 模型 ID" value={embeddingModelId} onChange={setEmbeddingModelId} models={models} disabled={disabled} required />
			<Field><FieldLabel htmlFor="context-budget-tokens">Context 预算 tokens</FieldLabel><Input id="context-budget-tokens" type="number" min="1" step="1" inputMode="numeric" value={contextBudgetTokens} onChange={(event) => setContextBudgetTokens(event.target.value)} disabled={disabled} required /></Field>
			<Field><FieldLabel htmlFor="max-output-tokens">最大输出 tokens</FieldLabel><Input id="max-output-tokens" type="number" min="1" step="1" inputMode="numeric" value={maxOutputTokens} onChange={(event) => setMaxOutputTokens(event.target.value)} disabled={disabled} required /></Field>
			<Button type="submit" disabled={disabled}>{saving ? "正在创建…" : "创建模型提供方"}</Button>
		</form>
	);
}
