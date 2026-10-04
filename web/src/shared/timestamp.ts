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

const relativeUnits: [Intl.RelativeTimeFormatUnit, number][] = [
  ["day", 86_400_000],
  ["hour", 3_600_000],
  ["minute", 60_000],
];

// Relative wording for ordinary timestamps only; extended and infinite values
// return undefined so callers fall back to formatTimestamp.
export function relativeTimestamp(
  value: string,
  now: number,
  locale: string,
): string | undefined {
  if (!/^\d{4}-/.test(value)) return undefined;
  const parsed = Date.parse(value);
  if (Number.isNaN(parsed)) return undefined;
  const elapsed = parsed - now;
  const format = new Intl.RelativeTimeFormat(locale, { numeric: "auto" });
  for (const [unit, size] of relativeUnits)
    if (Math.abs(elapsed) >= size)
      return format.format(Math.trunc(elapsed / size), unit);
  return format.format(Math.trunc(elapsed / 1000), "second");
}
