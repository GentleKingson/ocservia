const isoYearPrefix = /^\d{4}-/;
const isoFraction = /T\d{2}:\d{2}:\d{2}(\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;
const isoMillisZ = /\.\d{3}Z$/;
const maxCachedFormatters = 16;
const relativeFormatters = new Map<string, Intl.RelativeTimeFormat>();

function getRelativeFormatter(locale: string): Intl.RelativeTimeFormat {
  let formatter = relativeFormatters.get(locale);
  if (!formatter) {
    if (relativeFormatters.size >= maxCachedFormatters) {
      const oldest = relativeFormatters.keys().next().value;
      if (oldest !== undefined) relativeFormatters.delete(oldest);
    }
    formatter = new Intl.RelativeTimeFormat(locale, { numeric: "auto" });
    relativeFormatters.set(locale, formatter);
  }
  return formatter;
}

// Extended timestamps stay textual. Ordinary timestamps display in UTC,
// retaining their original fractional precision instead of Date milliseconds.
export function formatTimestamp(value: string): string {
  if (!isoYearPrefix.test(value)) return value;
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  const fraction = isoFraction.exec(value)?.[1] ?? "";
  return parsed
    .toISOString()
    .replace(isoMillisZ, fraction + "Z")
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
  if (!isoYearPrefix.test(value)) return undefined;
  const parsed = Date.parse(value);
  if (Number.isNaN(parsed)) return undefined;
  const elapsed = parsed - now;
  const format = getRelativeFormatter(locale);
  for (const [unit, size] of relativeUnits)
    if (Math.abs(elapsed) >= size)
      return format.format(Math.trunc(elapsed / size), unit);
  return format.format(Math.trunc(elapsed / 1000), "second");
}
