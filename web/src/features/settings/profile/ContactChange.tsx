import { LoaderCircle } from "lucide-react";
import { useEffect, useState } from "react";
import { messageOf, notify } from "@/app/shared";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
	InputOTP,
	InputOTPGroup,
	InputOTPSlot,
} from "@/components/ui/input-otp";
import {
	Select,
	SelectContent,
	SelectGroup,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import { channelLabels } from "@/features/settings/labels";
import {
	type ContactChangeFlow,
	completeContactChange,
	type OwnContact,
	readContactChangeFlow,
	sendFlowChallenge,
	stageFlowContact,
	startContactChange,
	verifyFlowChallenge,
} from "./api";

type Stage = "start" | "stage" | "send" | "verify" | "confirm" | "finished";

/** 管理员在资料页内联自助更换收码渠道（docs/authentication-design.md §1/§4）：
 * 旧渠道在原子完成前保持有效，任何一步失败都不影响现有登录能力；完成后所有会话撤销，
 * 整页重新加载回到登录页。当前密码只在请求瞬间使用，绝不持久化。调用方负责按角色渲染。 */
export function ContactChange({
	suspended,
	onClose,
}: {
	suspended: boolean;
	onClose: () => void;
}) {
	const [stage, setStage] = useState<Stage>("start");
	const [flow, setFlow] = useState<ContactChangeFlow>();
	const [candidate, setCandidate] = useState<OwnContact>();
	const [channel, setChannel] = useState<OwnContact["channel"]>("email");
	const [target, setTarget] = useState("");
	const [password, setPassword] = useState("");
	const [code, setCode] = useState("");
	const [busy, setBusy] = useState(false);

	// Completion revoked every session; a full reload guarantees a clean,
	// unauthenticated boot instead of a half-valid workbench.
	useEffect(() => {
		if (stage === "finished") window.location.reload();
	}, [stage]);

	const oldChannelPreserved = "原收码渠道保持不变，可重试或放弃本次变更。";

	async function run(action: () => Promise<void>) {
		setBusy(true);
		try {
			await action();
		} catch (reason) {
			// 失败提示带"旧渠道保持不变"的安抚语义，随 toast 一并呈现；
			// 面板标题也永久说明期间旧渠道可用。fallback 镜像完整文案，
			// 保证非 Error 原因（如校验字符串）也能带安抚语义。
			const detail = `${messageOf(reason, "暂时无法完成操作，请重试。")}${oldChannelPreserved}`;
			notify.error(detail, detail);
		} finally {
			setBusy(false);
		}
	}

	const start = () =>
		run(async () => {
			const next = await startContactChange(password);
			setFlow(next);
			setPassword("");
			setStage("stage");
		});

	const stageContact = () =>
		run(async () => {
			const masked = await stageFlowContact(channel, target.trim());
			setCandidate(masked);
			setTarget("");
			setStage("send");
		});

	const send = () =>
		run(async () => {
			if (!candidate) return;
			await sendFlowChallenge(candidate.id);
			setCode("");
			setStage("verify");
		});

	const verify = () =>
		run(async () => {
			// A 200 response is the proof for contact_change; `completed` stays
			// false here because it only denotes "logged in" on login flows.
			await verifyFlowChallenge(code);
			// The authoritative flow now carries passwordSet=true and the
			// verified candidate; re-read it instead of trusting local state.
			const refreshed = await readContactChangeFlow();
			if (!refreshed.passwordSet || !refreshed.candidate?.verified) {
				throw new Error("验证尚未生效，请重试。");
			}
			setFlow(refreshed);
			setCandidate(refreshed.candidate);
			setCode("");
			setStage("confirm");
		});

	const finish = () =>
		run(async () => {
			await completeContactChange(password);
			setPassword("");
			setStage("finished");
		});

	const abandon = () => {
		setStage("start");
		setFlow(undefined);
		setCandidate(undefined);
		setTarget("");
		setCode("");
		setPassword("");
		onClose();
	};

	const candidateBadge = candidate && (
		<Badge variant="outline">{candidate.maskedTarget}</Badge>
	);

	return (
		<div className="space-y-4 rounded-md border p-4">
			<div className="flex items-start justify-between gap-3">
				<div>
					<h3 className="font-medium">更换收码渠道</h3>
					<p className="text-sm text-muted-foreground">
						更换用于两步登录验证码的邮箱/短信目标。新渠道仅在验证通过并确认后生效；期间旧渠道保持可用。
					</p>
				</div>
				<Button variant="ghost" size="sm" onClick={abandon}>
					关闭
				</Button>
			</div>
			{stage === "start" && (
				<form
					className="flex max-w-sm flex-col gap-3"
					onSubmit={(event) => {
						event.preventDefault();
						void start();
					}}
				>
					<Field>
						<FieldLabel htmlFor="contact-change-password">当前密码</FieldLabel>
						<Input
							id="contact-change-password"
							type="password"
							autoComplete="current-password"
							value={password}
							disabled={busy || suspended}
							required
							onChange={(event) => setPassword(event.target.value)}
						/>
						<FieldDescription>
							密码证明只用于开启本次流程，不会被保存。
						</FieldDescription>
					</Field>
					<Button type="submit" disabled={busy || suspended || !password}>
						{busy ? (
							<>
								<LoaderCircle
									className="animate-spin"
									data-icon="inline-start"
									aria-hidden="true"
								/>
								开启中…
							</>
						) : (
							"开始更换"
						)}
					</Button>
				</form>
			)}
			{stage !== "start" && stage !== "finished" && flow && (
				<div className="flex flex-col gap-1 text-sm">
					<span className="text-muted-foreground">当前渠道：</span>
					<div className="flex flex-wrap gap-2">
						{(flow.contacts ?? []).map((contact) => (
							<Badge key={contact.id} variant="secondary">
								{channelLabels[contact.channel]} {contact.maskedTarget}
							</Badge>
						))}
					</div>
				</div>
			)}
			{stage === "stage" && (
				<div className="flex max-w-sm flex-col gap-3">
					<Field>
						<FieldLabel htmlFor="contact-change-channel">渠道</FieldLabel>
						<Select
							value={channel}
							onValueChange={(value) =>
								setChannel(value as OwnContact["channel"])
							}
						>
							<SelectTrigger id="contact-change-channel">
								<SelectValue />
							</SelectTrigger>
							<SelectContent>
								<SelectGroup>
									<SelectItem value="email">邮箱</SelectItem>
									<SelectItem value="sms">短信</SelectItem>
								</SelectGroup>
							</SelectContent>
						</Select>
						<FieldDescription>
							只能使用邮箱或短信；提交后先作为候选暂存，不立即生效。
						</FieldDescription>
					</Field>
					<Field>
						<FieldLabel htmlFor="contact-change-target">
							{channelLabels[channel]}目标
						</FieldLabel>
						<Input
							id="contact-change-target"
							type={channel === "email" ? "email" : "tel"}
							placeholder={
								channel === "email" ? "name@example.com" : "+8613800000000"
							}
							value={target}
							disabled={busy}
							onChange={(event) => setTarget(event.target.value)}
						/>
					</Field>
					<Button
						disabled={busy || !target.trim()}
						onClick={() => void stageContact()}
					>
						{busy ? (
							<>
								<LoaderCircle
									className="animate-spin"
									data-icon="inline-start"
									aria-hidden="true"
								/>
								暂存中…
							</>
						) : (
							"暂存新渠道"
						)}
					</Button>
					<Button variant="ghost" onClick={abandon}>
						放弃本次变更
					</Button>
				</div>
			)}
			{stage === "send" && (
				<div className="flex max-w-sm flex-col gap-3 text-sm">
					<div className="flex items-center gap-2">
						候选渠道：{candidateBadge}
						{candidate?.verified && <Badge>已验证</Badge>}
					</div>
					<p className="text-muted-foreground">
						向候选渠道发送一次性验证码以证明可以接收；旧渠道暂不改动。
					</p>
					<Button disabled={busy} onClick={() => void send()}>
						{busy ? (
							<>
								<LoaderCircle
									className="animate-spin"
									data-icon="inline-start"
									aria-hidden="true"
								/>
								发送中…
							</>
						) : (
							"发送验证码"
						)}
					</Button>
					<Button variant="ghost" onClick={() => setStage("stage")}>
						返回修改目标
					</Button>
				</div>
			)}
			{stage === "verify" && (
				<Field className="max-w-sm">
					<FieldLabel htmlFor="contact-change-code">验证码</FieldLabel>
					<div className="flex flex-wrap items-center gap-3">
						<InputOTP
							id="contact-change-code"
							maxLength={6}
							value={code}
							onChange={setCode}
							disabled={busy}
						>
							<InputOTPGroup>
								{Array.from({ length: 6 }, (_, index) => (
									<InputOTPSlot key={index} index={index} />
								))}
							</InputOTPGroup>
						</InputOTP>
						<Button
							disabled={busy || code.length < 6}
							onClick={() => void verify()}
						>
							{busy ? (
								<>
									<LoaderCircle
										className="animate-spin"
										data-icon="inline-start"
										aria-hidden="true"
									/>
									验证中…
								</>
							) : (
								"验证"
							)}
						</Button>
					</div>
					<FieldDescription>
						验证码短时有效且单次消费；失败次数跨重发累计。
					</FieldDescription>
				</Field>
			)}
			{stage === "confirm" && (
				<form
					className="flex max-w-sm flex-col gap-3"
					onSubmit={(event) => {
						event.preventDefault();
						void finish();
					}}
				>
					<div className="flex items-center gap-2 text-sm">
						已验证候选渠道：{candidateBadge}
					</div>
					<Field>
						<FieldLabel htmlFor="contact-change-confirm-password">
							再次输入当前密码
						</FieldLabel>
						<Input
							id="contact-change-confirm-password"
							type="password"
							autoComplete="current-password"
							value={password}
							disabled={busy || suspended}
							required
							onChange={(event) => setPassword(event.target.value)}
						/>
						<FieldDescription>
							确认后立即生效：所有会话（含本机）将被撤销并返回登录页。
						</FieldDescription>
					</Field>
					<Button type="submit" disabled={busy || suspended || !password}>
						{busy ? (
							<>
								<LoaderCircle
									className="animate-spin"
									data-icon="inline-start"
									aria-hidden="true"
								/>
								生效中…
							</>
						) : (
							"确认更换"
						)}
					</Button>
					<Button type="button" variant="ghost" onClick={abandon}>
						放弃本次变更
					</Button>
				</form>
			)}
			{stage === "finished" && (
				<p
					role="status"
					className="flex items-center gap-2 text-sm text-muted-foreground"
				>
					<LoaderCircle className="size-4 animate-spin" aria-hidden="true" />
					所有会话已撤销，正在返回登录页…
				</p>
			)}
		</div>
	);
}
