export interface AgentOption {
  id: string;
  label: string;
  summary: string;
}

export interface WorkspaceOption {
  id: string;
  instanceId?: string;
  name: string;
  path: string;
  icon?: string;
}

export interface ToastModel {
  message: string;
  revision: number;
}

export interface ConfirmDialogModel {
  open: boolean;
  revision: number;
  title: string;
  message: string;
  confirmLabel: string;
  cancelLabel: string;
  danger: boolean;
  onResult: (confirmed: boolean) => void;
}
