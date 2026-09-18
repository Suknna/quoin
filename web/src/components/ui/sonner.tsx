import {
	CircleCheckIcon,
	InfoIcon,
	Loader2Icon,
	OctagonXIcon,
	TriangleAlertIcon,
} from "lucide-react";
import { Toaster as Sonner, type ToasterProps } from "sonner";

// 项目没有运行时主题切换（ThemeProvider），主题由 <html> 上的 .dark 类决定，
// 因此直接读取 classList 而非引入 next-themes。
const currentTheme = (): ToasterProps["theme"] =>
	document.documentElement.classList.contains("dark") ? "dark" : "light";

const Toaster = ({ ...props }: ToasterProps) => {
	return (
		<Sonner
			theme={currentTheme()}
			// sonner 仅在 richColors 下把 success/warning/info/error 底色映射到 --*-bg；
			// 开启后下面的语义 token 才会生效，成功/失败/告警在视觉上可区分。
			richColors
			className="toaster group"
			icons={{
				success: <CircleCheckIcon className="size-4" />,
				info: <InfoIcon className="size-4" />,
				warning: <TriangleAlertIcon className="size-4" />,
				error: <OctagonXIcon className="size-4" />,
				loading: <Loader2Icon className="size-4 animate-spin" />,
			}}
			style={
				{
					"--normal-bg": "var(--popover)",
					"--normal-text": "var(--popover-foreground)",
					"--normal-border": "var(--border)",
					"--border-radius": "var(--radius)",
					// 状态变体色复用 theme.css 已定义的语义 token，亮/暗主题自动适配；
					// --success 没有配套 foreground，近白文字在深/浅绿底上均可读。
					"--success-bg": "var(--success)",
					"--success-text": "oklch(0.985 0 0)",
					"--success-border": "var(--success)",
					"--warning-bg": "var(--warning)",
					"--warning-text": "var(--warning-foreground)",
					"--warning-border": "var(--warning)",
					"--info-bg": "var(--info)",
					"--info-text": "var(--info-foreground)",
					"--info-border": "var(--info)",
					"--error-bg": "var(--destructive)",
					"--error-text": "var(--destructive-foreground)",
					"--error-border": "var(--destructive)",
				} as React.CSSProperties
			}
			{...props}
		/>
	);
};

export { Toaster };
