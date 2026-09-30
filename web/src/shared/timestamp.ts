// Extended timestamps stay textual. Ordinary timestamps display in UTC,
// retaining their original fractional precision instead of Date milliseconds.
export function formatTimestamp(value: string): string {
  if (!/^\d{4}-/.test(value)) return value;
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  const fraction =
    /T\d{2}:\d{2}:\d{2}(\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.exec(value)?.[1] ?? "";
  return parsed
    .toISOString()
    .replace(/\.\d{3}Z$/, fraction + "Z")
    .replace("T", " ")
    .replace(/Z$/, " UTC");
}
