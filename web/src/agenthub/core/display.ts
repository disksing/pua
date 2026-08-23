export function displayTime(value: string | undefined): string {
  const date = new Date(value || "");
  return Number.isNaN(date.valueOf())
    ? ""
    : date.toLocaleTimeString("en-US", {
        hour: "2-digit",
        minute: "2-digit",
      });
}

// Renders an elapsed span such as "12 seconds" or "1m2s". Returns "" when
// either endpoint is missing or not a valid timestamp.
export function displayDuration(start: string | undefined, end: string | undefined): string {
  const from = new Date(start || "").valueOf();
  const to = new Date(end || "").valueOf();
  if (Number.isNaN(from) || Number.isNaN(to) || to < from) return "";
  const seconds = Math.round((to - from) / 1000);
  if (seconds < 60) return `${seconds} ${seconds === 1 ? "second" : "seconds"}`;
  return `${Math.floor(seconds / 60)}m${seconds % 60}s`;
}
