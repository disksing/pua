export interface SessionSource {
  app?: string;
  instanceId?: string;
  externalId?: string;
  metadata?: Record<string, string>;
}

export interface AgentHubSession {
  id: string;
  title: string;
  cwd: string;
  agentName?: string;
  source?: SessionSource;
  provider?: string;
  providerSessionId?: string;
  state: "starting" | "ready" | "running" | "waiting_approval" | "stopping" | "stopped" | "archived" | string;
  stopReason?: string;
  currentTurnId?: string;
  pendingApprovalIds?: string[];
  inputCapabilities?: { steer?: boolean };
  lastEventId: number;
  lastActivityAt?: string;
  createdAt: string;
  updatedAt: string;
}

export interface AgentHubTurnSummary {
  id: string;
  turnId: string;
  status: string;
  closed: boolean;
  startedAt: string;
  endedAt?: string;
  durationMs: number;
  triggerPreview?: string;
  finalReplyPreview?: string;
  eventCount: number;
  toolEventCount: number;
  firstEventId: number;
  lastEventId: number;
}

export interface SessionListPage {
  sessions: AgentHubSession[];
  page: { limit: number; nextCursor?: string; hasMore: boolean };
}

export interface SessionFilters {
  archived: boolean;
  states: string[];
  sourceApp: string;
  sourceInstanceId: string;
  sourceExternalId: string;
}

export interface AgentHubProvider {
  id: string;
  name: string;
  type: string;
  enabled: boolean;
  command?: string;
}

export interface AgentHubAgent {
  name: string;
  providerId: string;
  options?: Record<string, string>;
  environment?: Record<string, string>;
  available?: boolean;
  unavailableReason?: string;
}

export interface AgentHubProbe {
  providerId: string;
  available: boolean;
  command?: string;
  error?: string;
}

export interface OnWatchConfig {
  enabled: boolean;
  serverUrl: string;
  authMode: string;
  username: string;
  password: string;
  refreshIntervalSeconds: number;
}

export interface AgentHubConfig {
  version: number;
  agentProviders: AgentHubProvider[];
  agents: AgentHubAgent[];
  onWatch: OnWatchConfig;
}

export interface AgentCatalog {
  providers: AgentHubProvider[];
  agents: AgentHubAgent[];
  probes: AgentHubProbe[];
}

export interface SemanticEvent {
  id: string;
  sourceEventId: number;
  index: number;
  type: string;
  time?: string;
  startTime?: string;
  sessionId?: string;
  turnId?: string;
  data?: Record<string, any>;
}

export interface SemanticFrame {
  schema: "agenthub.semantic-events.v1";
  cursor: number;
  mode: "replace" | "append";
  source: { eventId: number; type: string; sessionId: string; turnId?: string; time?: string; startTime?: string };
  events: SemanticEvent[];
}

export interface ActivityTurnTerminal {
  turnId?: string;
  status: "completed" | "failed" | "cancelled";
  endedAt: string;
}

export interface ActivitySession {
  sessionId: string;
  provider?: string;
  title?: string;
  turnId?: string;
  eventCount: number;
  completed: boolean;
  turnTerminal?: ActivityTurnTerminal;
  lastEventAt: string;
  [key: string]: any;
}

export interface ActivityFrame {
  sequence: number;
  windowStartedAt: string;
  windowEndedAt: string;
  sessions: ActivitySession[];
}
