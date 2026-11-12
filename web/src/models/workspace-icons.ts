export interface WorkspaceIconOption {
	id: string;
	label: string;
	src: string;
	faviconSrc?: string;
	type?: string;
}

export const DEFAULT_WORKSPACE_ICON: WorkspaceIconOption = {
	id: "",
	label: "PUA default",
	src: "/workspace-icons/pua-yellow.png",
	faviconSrc: "/workspace-icons/pua-yellow-opaque.png",
	type: "image/png",
};

export const WORKSPACE_ICONS: WorkspaceIconOption[] = [
	DEFAULT_WORKSPACE_ICON,
	{ id: "pua-red", label: "PUA red", src: "/workspace-icons/pua-red.png", faviconSrc: "/workspace-icons/pua-red-opaque.png" },
	{ id: "pua-green", label: "PUA green", src: "/workspace-icons/pua-green.png", faviconSrc: "/workspace-icons/pua-green-opaque.png" },
	{ id: "pua-blue", label: "PUA blue", src: "/workspace-icons/pua-blue.png", faviconSrc: "/workspace-icons/pua-blue-opaque.png" },
	{ id: "pua-purple", label: "PUA purple", src: "/workspace-icons/pua-purple.png", faviconSrc: "/workspace-icons/pua-purple-opaque.png" },
	{ id: "home-base", label: "Home base", src: "/workspace-icons/01-home-base.png" },
	{ id: "personal-tasks", label: "Personal tasks", src: "/workspace-icons/02-personal-tasks.png" },
	{ id: "product-roadmap", label: "Product roadmap", src: "/workspace-icons/03-product-roadmap.png" },
	{ id: "software-engineering", label: "Software engineering", src: "/workspace-icons/04-software-engineering.png" },
	{ id: "design-studio", label: "Design studio", src: "/workspace-icons/05-design-studio.png" },
	{ id: "marketing-campaign", label: "Marketing campaign", src: "/workspace-icons/06-marketing-campaign.png" },
	{ id: "sales-pipeline", label: "Sales pipeline", src: "/workspace-icons/07-sales-pipeline.png" },
	{ id: "operations", label: "Operations", src: "/workspace-icons/08-operations.png" },
	{ id: "finance", label: "Finance", src: "/workspace-icons/09-finance.png" },
	{ id: "research-lab", label: "Research lab", src: "/workspace-icons/10-research-lab.png" },
	{ id: "learning-education", label: "Learning and education", src: "/workspace-icons/11-learning-education.png" },
	{ id: "customer-support", label: "Customer support", src: "/workspace-icons/12-customer-support.png" },
	{ id: "events-calendar", label: "Events and calendar", src: "/workspace-icons/13-events-calendar.png" },
	{ id: "documentation-knowledge", label: "Documentation and knowledge", src: "/workspace-icons/14-documentation-knowledge.png" },
	{ id: "analytics", label: "Analytics", src: "/workspace-icons/15-analytics.png" },
	{ id: "community-team", label: "Community and team", src: "/workspace-icons/16-community-team.png" },
];

export const WORKSPACE_ICON_BY_ID = new Map(WORKSPACE_ICONS.map((item) => [item.id, item]));

export function workspaceIconOption(workspace: { icon?: string } | null | undefined): WorkspaceIconOption {
	return WORKSPACE_ICON_BY_ID.get(String(workspace?.icon || "").trim()) || DEFAULT_WORKSPACE_ICON;
}
