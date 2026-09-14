import { cn } from "@/lib/utils";

/**
 * Runtime brand assets served from web/public/brand. Raster PNGs exported from
 * material/ready (see docs/frontend/brand/runtime-assets.yaml for provenance);
 * there is no vector source, so themes pick between pre-rendered variants.
 */
export const BRAND_ASSET_FILES = [
	"favicon.ico",
	"icon-16.png",
	"icon-32.png",
	"icon-48.png",
	"icon-180.png",
	"icon-192.png",
	"app-icon-512.png",
	"lockup-light.png",
	"lockup-dark.png",
	"mark-light.png",
	"mark-dark.png",
	"illustration-core.png",
	"evidence-monitoring.png",
	"evidence-events.png",
	"evidence-knowledge.png",
	"investigation-record.png",
] as const;

type BrandBackground = "light" | "dark";

const LOCKUP_SRC: Record<BrandBackground, string> = {
	light: "/brand/lockup-light.png",
	dark: "/brand/lockup-dark.png",
};

/**
 * Quoin lockup (mark + wordmark) as an accessible image.
 *
 * `background="adaptive"` renders both pre-rendered variants and lets the
 * `.dark` class choose, so the visible bitmap always matches the theme. The
 * wrapper carries the accessible name because only one variant is visible at
 * a time.
 */
export function BrandLockup({
	background = "adaptive",
	alt = "Quoin",
	className,
}: {
	background?: BrandBackground | "adaptive";
	alt?: string;
	className?: string;
}) {
	if (background !== "adaptive") {
		return (
			<img
				src={LOCKUP_SRC[background]}
				alt={alt}
				className={cn("object-contain", className)}
			/>
		);
	}
	return (
		<span role="img" aria-label={alt} className={cn("inline-flex", className)}>
			<img
				src={LOCKUP_SRC.light}
				alt=""
				className="h-full w-auto object-contain dark:hidden"
			/>
			<img
				src={LOCKUP_SRC.dark}
				alt=""
				className="hidden h-full w-auto object-contain dark:block"
			/>
		</span>
	);
}

/**
 * Standalone Quoin mark for compact surfaces (sidebar rail). Decorative:
 * the surrounding control owns the accessible name.
 */
export function BrandMark({ className }: { className?: string }) {
	return (
		<>
			<img
				src="/brand/mark-light.png"
				alt=""
				aria-hidden="true"
				className={cn("object-contain dark:hidden", className)}
			/>
			<img
				src="/brand/mark-dark.png"
				alt=""
				aria-hidden="true"
				className={cn("hidden object-contain dark:block", className)}
			/>
		</>
	);
}
