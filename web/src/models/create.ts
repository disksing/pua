import type { AgentOption } from "./common";
import type { ResourceAgentBindingModel, ResourceAgentProfileModel } from "./detail";

const resourceSlugPattern = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

export function isValidResourceSlug(value: string): boolean {
  const slug = value.trim();
  return slug === "" || resourceSlugPattern.test(slug);
}

export interface CreateDraft {
  type: "project" | "task";
  projectId: string;
  title: string;
  description: string;
  detail: string;
  slug: string;
  // Start options (wizard final step): create the task and immediately switch
  // the agent binding (when it differs from the resolved default) and send the
  // start prompt as the first message.
  startAfterCreate: boolean;
  startBinding: ResourceAgentBindingModel;
  startPrompt: string;
}

export interface CreateDialogModel {
  open: boolean;
  identity: string;
  workspaceId: string;
  draft: CreateDraft;
  submitting: boolean;
  agents: AgentOption[];
  agentProfiles: ResourceAgentProfileModel[];
  // Binding a newly created task would resolve to (project task default when
  // set, otherwise the workspace task default). The wizard preselects it and
  // only rebinds the created task when the user picks something else.
  defaultTaskBinding: ResourceAgentBindingModel;
  onClose: () => void;
  onSubmit: (draft: CreateDraft) => Promise<void>;
}
