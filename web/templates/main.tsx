import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import LoginPage from "./login-02";
import SidebarPage from "./sidebar-09";
import "./styles.css";

const theme = new URLSearchParams(window.location.search).get("theme");
document.documentElement.classList.toggle(
	"dark",
	theme === "dark" ||
		(theme !== "light" &&
			window.matchMedia("(prefers-color-scheme: dark)").matches),
);

// Template previews must never submit credentials or impersonate real authentication.
document.addEventListener("submit", (event) => event.preventDefault(), true);

const root = document.getElementById("root");
if (!root) throw new Error("Template root is missing");

createRoot(root).render(
	<StrictMode>
		{window.location.pathname.startsWith("/login-02") ? (
			<LoginPage />
		) : (
			<SidebarPage />
		)}
	</StrictMode>,
);
