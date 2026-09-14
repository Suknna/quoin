import { BrandLockup } from "./Brand";

/**
 * The five exported brand bitmaps (see material/ready/README.md) composed in a
 * fixed 512x352 design space. Percentage geometry keeps the SVG connectors and
 * the raster nodes aligned at any rendered width; captions stay real text so
 * the labels are never baked into a bitmap.
 */
const SOURCE_NODES = [
	{
		label: "监控指标",
		asset: "/brand/evidence-monitoring.png",
		top: "9.2%",
		delay: 300,
	},
	{
		label: "事件线索",
		asset: "/brand/evidence-events.png",
		top: "39.9%",
		delay: 440,
	},
	{
		label: "历史知识",
		asset: "/brand/evidence-knowledge.png",
		top: "70.5%",
		delay: 580,
	},
] as const;

const CONNECTOR_PATHS = [
	"M205 68C222 68 214 176 230 176",
	"M205 176H230",
	"M205 284C222 284 214 176 230 176",
] as const;

function EvidenceIllustration() {
	return (
		<div
			aria-hidden="true"
			className="relative mx-auto aspect-[16/11] w-full max-w-[512px]"
		>
			<svg
				viewBox="0 0 512 352"
				fill="none"
				className="absolute inset-0 h-full w-full"
				focusable="false"
			>
				<g className="stroke-border" strokeWidth="1.5">
					{CONNECTOR_PATHS.map((d, index) => (
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
					d="M344 176H388"
					pathLength="1"
					className="auth-brand-link stroke-chart-1/60"
					strokeWidth="1.5"
					style={{ animationDelay: "1000ms" }}
				/>
				<circle
					cx="234"
					cy="176"
					r="4"
					className="auth-brand-enter fill-chart-1"
					style={{ animationDelay: "900ms" }}
				/>
			</svg>
			{SOURCE_NODES.map(({ label, asset, top, delay }) => (
				<div
					key={asset}
					className="auth-brand-enter absolute left-0 flex w-[40%] flex-col gap-1.5"
					style={{ top, animationDelay: `${delay}ms` }}
				>
					<img src={asset} alt="" className="w-full object-contain" />
					<span className="text-xs text-muted-foreground">{label}</span>
				</div>
			))}
			<div
				className="auth-brand-enter absolute left-[46.1%] top-[35.2%] w-[20.3%]"
				style={{ animationDelay: "700ms" }}
			>
				<img
					src="/brand/illustration-core.png"
					alt=""
					className="w-full object-contain"
				/>
			</div>
			<div
				className="auth-brand-enter absolute left-[75.8%] top-[30.7%] flex w-[24.2%] flex-col gap-1.5"
				style={{ animationDelay: "1000ms" }}
			>
				<img
					src="/brand/investigation-record.png"
					alt=""
					className="w-full object-contain"
				/>
				<span className="text-xs text-muted-foreground">调查记录</span>
			</div>
		</div>
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
						<BrandLockup background="dark" alt="quoin" className="h-4" />
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
