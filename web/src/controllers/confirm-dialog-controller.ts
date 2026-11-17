import { createModelChannel, type ModelChannel } from "../components/model-channel";
import type { ConfirmDialogModel } from "../models/common";

export interface ConfirmDialogOptions {
	title?: string;
	message: string;
	confirmLabel?: string;
	cancelLabel?: string;
	danger?: boolean;
	defaultToCancel?: boolean;
}

export interface ConfirmDialogController {
	channel: ModelChannel<ConfirmDialogModel>;
	confirm(options: ConfirmDialogOptions): Promise<boolean>;
}

const noop = () => undefined;

export function createConfirmDialogController(): ConfirmDialogController {
	const channel = createModelChannel<ConfirmDialogModel>({
		open: false,
		revision: 0,
		title: "",
		message: "",
		confirmLabel: "Confirm",
		cancelLabel: "Cancel",
		danger: false,
		onResult: noop
	});
	let pending: ((confirmed: boolean) => void) | null = null;

	function settle(confirmed: boolean): void {
		const resolve = pending;
		pending = null;
		const current = channel.current();
		if (current.open) channel.publish({ ...current, open: false });
		if (resolve) resolve(confirmed);
	}

	function confirm(options: ConfirmDialogOptions): Promise<boolean> {
		// Only one confirmation can be visible at a time; cancel any previous one.
		settle(false);
		return new Promise<boolean>((resolve) => {
			pending = resolve;
			channel.publish({
				open: true,
				revision: channel.current().revision + 1,
				title: options.title?.trim() || "Please confirm",
				message: options.message,
				confirmLabel: options.confirmLabel?.trim() || "Confirm",
				cancelLabel: options.cancelLabel?.trim() || "Cancel",
				danger: Boolean(options.danger),
				defaultToCancel: options.defaultToCancel,
				onResult: settle
			});
		});
	}

	return { channel, confirm };
}

const shared = createConfirmDialogController();

export const confirmDialogChannel = shared.channel;

export function confirmDialog(options: ConfirmDialogOptions): Promise<boolean> {
	return shared.confirm(options);
}
