import { Activity, BookOpen, FileSearch, ListTree } from "lucide-react";

function EvidenceIllustration() {
	return (
		<svg
			viewBox="0 0 480 260"
			fill="none"
			className="w-full max-w-md"
			aria-hidden="true"
			focusable="false"
		>
			<g className="stroke-border" strokeWidth="1.5">
				{[
					"M148 58H180Q204 58 204 82V106Q204 130 228 130H272",
					"M148 130H272",
					"M148 202H180Q204 202 204 178V154Q204 130 228 130H272",
				].map((d, index) => (
					<path
						key={d}
						d={d}
						pathLength="1"
						className="auth-brand-link"
						style={{ animationDelay: `${550 + index * 140}ms` }}
					/>
				))}
			</g>
			<path
				d="M228 130H272"
				pathLength="1"
				className="auth-brand-link stroke-chart-1/60"
				strokeWidth="1.5"
				style={{ animationDelay: "1000ms" }}
			/>
			<circle
				cx="228"
				cy="130"
				r="4"
				className="auth-brand-enter fill-chart-1"
				style={{ animationDelay: "900ms" }}
			/>
			{[
				{ label: "监控指标", icon: Activity, y: 34 },
				{ label: "事件线索", icon: ListTree, y: 106 },
				{ label: "历史知识", icon: BookOpen, y: 178 },
			].map(({ label, icon: Icon, y }, index) => (
				<g key={label} transform={`translate(8 ${y})`}>
					<g
						className="auth-brand-enter"
						style={{ animationDelay: `${300 + index * 140}ms` }}
					>
						<rect
							width="140"
							height="48"
							rx="10"
							className="fill-card stroke-border"
						/>
						<Icon
							x="17"
							y="15"
							width="18"
							height="18"
							strokeWidth="1.5"
							className="text-muted-foreground"
						/>
						<text
							x="48"
							y="29"
							fontSize="13"
							className="fill-current text-foreground/85"
						>
							{label}
						</text>
					</g>
				</g>
			))}
			<g className="auth-brand-enter" style={{ animationDelay: "1000ms" }}>
				<rect
					x="280"
					y="62"
					width="184"
					height="152"
					rx="12"
					className="stroke-border"
				/>
				<rect
					x="272"
					y="54"
					width="184"
					height="152"
					rx="12"
					className="fill-card stroke-border"
				/>
				<FileSearch
					x="292"
					y="75"
					width="20"
					height="20"
					strokeWidth="1.5"
					className="text-chart-1"
				/>
				<text
					x="324"
					y="90"
					fontSize="14"
					fontWeight="500"
					className="fill-current"
				>
					调查记录
				</text>
				<path d="M292 111H436" className="stroke-border" />
				<g
					className="stroke-muted-foreground/30"
					strokeLinecap="round"
					strokeWidth="3"
				>
					<path d="M293 131H421" />
					<path d="M293 145H390" />
				</g>
				<text
					x="292"
					y="182"
					fontSize="11"
					className="fill-current text-muted-foreground"
				>
					证据关联 · 分析留痕
				</text>
			</g>
		</svg>
	);
}

export function AuthBrandPanel() {
	return (
		<aside
			aria-label="关于 Quoin"
			className="auth-brand-panel hidden min-w-0 flex-col bg-background px-12 py-10 text-foreground lg:flex xl:px-16 2xl:px-20"
		>
			<div className="mx-auto flex w-full max-w-lg flex-1 flex-col justify-center gap-10 py-10 2xl:gap-14">
				<div className="flex flex-col gap-6">
					<p className="auth-brand-enter flex items-center gap-3 text-xs text-muted-foreground">
						<span className="font-medium tracking-[0.2em]">quoin</span>
						<span aria-hidden="true" className="text-muted-foreground/50">
							|
						</span>
						<span className="tracking-widest">运维工作台</span>
					</p>
					<h2
						className="auth-brand-enter text-[clamp(2rem,3.15vw,3rem)] leading-[1.4] font-medium tracking-tight"
						style={{ animationDelay: "120ms" }}
					>
						<span className="block">让每次一判断，</span>
						<span className="block">都有据可寻。</span>
					</h2>
				</div>
				<EvidenceIllustration />
			</div>
		</aside>
	);
}
