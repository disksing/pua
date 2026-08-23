export interface ApiErrorResponse {
  error?: string;
  code?: string;
  [key: string]: unknown;
}

export interface ArchiveWarning {
  severity?: "warning" | string;
  code: string;
  message: string;
  resourceId?: string;
  repo?: string;
  path?: string;
  branch?: string;
  targetBranch?: string;
}

export interface ArchiveResponse {
  path: string;
  warnings?: ArchiveWarning[];
}

export interface WorkspaceSummary {
  id: string;
  name: string;
  path: string;
  icon?: string;
}

export type ResourceType = "scheduler" | "project" | "task";

export interface ResourceSummary {
  id: string;
  type: ResourceType;
  title: string;
  path: string;
  archived: boolean;
  children?: ResourceSummary[];
}

export interface WorkspaceTreeResponse {
  root: string;
  workspace?: ResourceSummary;
  scheduler?: ResourceSummary;
  projects: ResourceSummary[];
}

export interface ServiceReadinessStatus {
  configured: boolean;
  ready: boolean;
  lastCheck?: string;
  lastError?: string;
}

export interface ServiceCleanupStatus {
  configured: boolean;
  attempts?: number;
  lastRun?: string;
  lastError?: string;
  succeeded: boolean;
}

export interface ServiceExportMetadata {
  variables?: Record<string, string>;
  secrets?: Array<{ name: string; source?: string; updatedAt?: string }>;
  updatedAt?: string;
}

export type ServiceState =
  | "disabled"
  | "stopped"
  | "blocked"
  | "starting"
  | "ready"
  | "backoff"
  | "attention_required";

export interface ServiceStatus {
  id: string;
  enabled: boolean;
  state: ServiceState;
  pid?: number;
  failureCount?: number;
  nextRetryAt?: string;
  lastError?: string;
  attentionRequired?: boolean;
  dependencies?: string[];
  readiness: ServiceReadinessStatus;
  cleanup: ServiceCleanupStatus;
  exports: ServiceExportMetadata;
}
