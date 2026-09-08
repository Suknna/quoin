import type { UserSummary } from "../src/api/generated/types";
import type {
	ConnectionDetailView,
	ProbeAttemptView,
	ProbeResultView,
} from "./api";

/** Contract fixtures contain no secrets and use the generated auth schema verbatim. */
export const authUser: UserSummary = {
	id: "1",
	username: "admin",
	displayName: "Admin",
	role: "admin",
	enabled: true,
	passwordChangeRequired: false,
	authRevision: 1,
	rowVersion: 1,
	lastLoginAt: null,
};
export const otherUser: UserSummary = {
	...authUser,
	id: "2",
	username: "operator",
	displayName: "Operator",
	role: "operator",
};
export const queuedProbe: ProbeAttemptView = {
	id: "42",
	state: "Queued",
	type: "connection_probe",
	rowVersion: 2,
	createdAt: "2026-09-08T00:00:00Z",
};
export const rowVersionConflict = {
	code: "row_version_conflict",
	detail: "stale",
};
export const runningAttempt: ProbeAttemptView = {
	id: "42",
	type: "connection_probe",
	state: "Running",
	rowVersion: 3,
	createdAt: "2026-09-08T00:00:00Z",
};
export const failedResult: ProbeResultView = {
	id: "9",
	attemptId: "42",
	connectionType: "thanos",
	connectionRevisionId: "3",
	credentialGenerationId: "4",
	rootBindingRevision: 1,
	actionSetId: "thanos",
	actionSetVersion: 1,
	probeContractDigest: "contract",
	outcome: "failed",
	resultDigest: "digest",
	startedAt: "2026-09-08T00:00:00Z",
	finishedAt: "2026-09-08T00:01:00Z",
	details: { reason: "fixture failure" },
};
export const modelProviderDetail: ConnectionDetailView = {
	name: "models/main",
	type: "model_provider",
	enabled: false,
	revalidationRequired: false,
	currentRevisionId: "11",
	currentCredentialGenerationId: "12",
	rowVersion: 3,
	config: {
		type: "model_provider",
		baseUrl: "https://provider.invalid",
		chatModelId: "chat-1",
		contextBudgetTokens: 8192,
		maxOutputTokens: 1024,
	},
	revisionCount: 1,
	generationCount: 1,
};

export const thanosDetail: ConnectionDetailView = {
	name: "thanos",
	type: "thanos",
	enabled: false,
	revalidationRequired: false,
	rowVersion: 2,
	config: { type: "thanos", baseUrl: "https://thanos.example" },
	revisionCount: 1,
	generationCount: 1,
};
