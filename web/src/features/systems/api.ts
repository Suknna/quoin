// Business views are the optional, versioned scope-and-description organization
// introduced by ADR 0004. Views never own credentials or query permissions; the
// server stays authoritative for validation and optimistic concurrency.

import { formatDateTime } from "@/lib/format";
import { newClientCommandId, request } from "@/api/workbench";

export interface BusinessViewScope {
  /** Omitted means "candidate sources are all integrations"; purely descriptive, grants nothing. */
  connectionName?: string;
  labelConditions: Record<string, string>;
  /**
   * Explicit Alertmanager source keys for alert attribution (ADR-0008). Empty
   * or omitted means the view never participates in alert attribution; the
   * Prometheus connection identity above is never substituted for it.
   */
  alertSourceKeys?: string[];
}

export interface BusinessView {
  viewKey: string;
  displayName: string;
  description: string;
  scope: BusinessViewScope;
  rowVersion: number;
  createdAt: string;
  updatedAt: string;
}

export async function listBusinessViews(): Promise<BusinessView[]> {
  const page = await request<{ items?: BusinessView[] }>("/api/v1/business-views");
  return page.items ?? [];
}

/** Session-level key/name projection for alert filtering: the list endpoint is
 * readable by every signed-in session and projects only the filter-safe fields
 * for non-admin callers, so this type deliberately carries nothing more. */
export interface BusinessViewOption {
  viewKey: string;
  displayName: string;
}

export async function listBusinessViewOptions(): Promise<BusinessViewOption[]> {
  const page = await request<{ items?: { viewKey?: string; displayName?: string }[] }>(
    "/api/v1/business-views",
  );
  return (page.items ?? [])
    .filter((item): item is { viewKey: string; displayName?: string } =>
      Boolean(item.viewKey))
    .map((item) => ({
      viewKey: item.viewKey,
      displayName: item.displayName || item.viewKey,
    }));
}

export function getBusinessView(viewKey: string): Promise<BusinessView> {
  return request<BusinessView>(`/api/v1/business-views/${encodeURIComponent(viewKey)}`);
}

export function createBusinessView(input: {
  viewKey: string;
  displayName: string;
  description: string;
  scope: BusinessViewScope;
}): Promise<BusinessView> {
  return request<BusinessView>("/api/v1/business-views", {
    method: "POST",
    body: JSON.stringify({ clientCommandId: newClientCommandId(), ...input }),
  });
}

/** Mutating commands fence on the row version the editor last read. */
export function updateBusinessView(
  viewKey: string,
  input: { displayName: string; description: string; scope: BusinessViewScope },
  expectedRowVersion: number,
): Promise<BusinessView> {
  return request<BusinessView>(`/api/v1/business-views/${encodeURIComponent(viewKey)}`, {
    method: "PUT",
    body: JSON.stringify({ clientCommandId: newClientCommandId(), ...input, expectedRowVersion }),
  });
}

export function formatTime(timestamp: string): string {
  return formatDateTime(timestamp, timestamp);
}
