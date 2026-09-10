// Extended timestamps stay textual; Date is used only to display ordinary years.
export function formatTimestamp(value: string): string {
  if (!/^\d{4}-/.test(value)) return value;
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}
