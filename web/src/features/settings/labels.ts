import type { UserSummary } from "@/api/generated/types";

/** Shared presentation labels for settings surfaces; raw enums stay internal. */
export const roleLabels: Record<UserSummary["role"], string> = {
	admin: "管理员",
	operator: "操作员",
};

export const channelLabels = { email: "邮箱", sms: "短信" } as const;
